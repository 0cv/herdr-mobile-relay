package portmap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	ssdpMulticastAddr = "239.255.255.250:1900"
	ssdpDefaultWait   = 2500 * time.Millisecond
	ssdpMaxDatagram   = 2048
	ssdpMaxResponses  = 8
	ssdpMX            = "2"

	maxDescriptionBytes = 256 << 10
	maxSOAPBytes        = 64 << 10
	maxDeviceDepth      = 8

	upnpDescription = "herdr-mobile-relay"
)

// upnpSearchTargets are the SSDP search targets that find an IGD root device.
var upnpSearchTargets = []string{
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
}

// upnpServiceTypes are the connection services that expose AddPortMapping, in
// preference order.
var upnpServiceTypes = []string{
	"urn:schemas-upnp-org:service:WANIPConnection:2",
	"urn:schemas-upnp-org:service:WANIPConnection:1",
	"urn:schemas-upnp-org:service:WANPPPConnection:1",
}

// UPnP IGD error codes we act on.
const (
	upnpErrConflictingMapping    = 718
	upnpErrOnlyPermanentLeases   = 725
	upnpEphemeralPortBase        = 49152
	upnpEphemeralPortCount       = 65536 - upnpEphemeralPortBase
	upnpConflictRetryPortAttempt = 1
)

// upnpError reports a SOAP fault or an unexpected HTTP status from the router.
type upnpError struct {
	Status      int
	Code        int
	Description string
}

func (e *upnpError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("upnp error %d (%s)", e.Code, e.Description)
	}
	return fmt.Sprintf("upnp http status %d", e.Status)
}

// upnpTarget is a validated control endpoint on the discovered gateway.
type upnpTarget struct {
	gateway netip.Addr
	control *url.URL
	service string
}

func newUPnPHTTPClient() *http.Client {
	return &http.Client{
		Timeout: stepTimeout,
		Transport: &http.Transport{
			// A router lives on the LAN: never route these through a proxy.
			Proxy:                 nil,
			DisableKeepAlives:     true,
			DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			ResponseHeaderTimeout: stepTimeout,
		},
	}
}

// routerAddrAllowed keeps discovery pointed at the local network. It is the
// SSRF guard for everything derived from router-supplied URLs.
func routerAddrAllowed(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast()
}

// hostAddr returns the URL host as an IP. Host names are rejected on purpose:
// a router must not be able to point us at a resolvable third party.
func hostAddr(u *url.URL) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(u.Hostname())
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// parseSSDPLocation extracts the LOCATION header of an SSDP search response.
func parseSSDPLocation(datagram []byte) (*url.URL, bool) {
	reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(datagram)))
	status, err := reader.ReadLine()
	if err != nil {
		return nil, false
	}
	if !strings.HasPrefix(strings.ToUpper(status), "HTTP/1.1 200") {
		return nil, false
	}
	headers, err := reader.ReadMIMEHeader()
	if err != nil && len(headers) == 0 {
		return nil, false
	}
	location := strings.TrimSpace(headers.Get("Location"))
	if location == "" {
		return nil, false
	}
	parsed, err := url.Parse(location)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		return nil, false
	}
	return parsed, true
}

// ssdpSearch multicasts an M-SEARCH and returns the description URLs of the
// routers that answered, keyed by the address they answered from.
func (c *client) ssdpSearch(ctx context.Context) (map[netip.Addr]*url.URL, error) {
	multicast, err := net.ResolveUDPAddr("udp4", c.ssdpAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve ssdp address: %w", err)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("open ssdp socket: %w", err)
	}
	defer conn.Close()

	wait := c.ssdpWait
	if wait <= 0 {
		wait = ssdpDefaultWait
	}
	deadline := time.Now().Add(wait)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}

	for _, target := range upnpSearchTargets {
		search := "M-SEARCH * HTTP/1.1\r\n" +
			"HOST: " + c.ssdpAddr + "\r\n" +
			"MAN: \"ssdp:discover\"\r\n" +
			"MX: " + ssdpMX + "\r\n" +
			"ST: " + target + "\r\n\r\n"
		if _, err := conn.WriteToUDP([]byte(search), multicast); err != nil {
			return nil, fmt.Errorf("send ssdp search: %w", err)
		}
	}

	found := make(map[netip.Addr]*url.URL)
	buf := make([]byte, ssdpMaxDatagram)
	for len(found) < ssdpMaxResponses {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		source, ok := netip.AddrFromSlice(from.IP)
		if !ok {
			continue
		}
		source = source.Unmap()
		if !routerAddrAllowed(source) {
			continue
		}
		if _, dup := found[source]; dup {
			continue
		}
		location, ok := parseSSDPLocation(buf[:n])
		if !ok {
			continue
		}
		// The description must live on the box that answered.
		if addr, ok := hostAddr(location); !ok || addr != source {
			c.logger.Debug("ignoring ssdp response with off-host location", "source", source)
			continue
		}
		found[source] = location
	}
	if len(found) == 0 {
		return nil, errors.New("no ssdp responses")
	}
	return found, nil
}

// upnpRoot mirrors the parts of an IGD device description we need.
type upnpRoot struct {
	XMLName xml.Name   `xml:"root"`
	URLBase string     `xml:"URLBase"`
	Device  upnpDevice `xml:"device"`
}

type upnpDevice struct {
	DeviceType string        `xml:"deviceType"`
	Services   []upnpService `xml:"serviceList>service"`
	Devices    []upnpDevice  `xml:"deviceList>device"`
}

type upnpService struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
}

// collectServices walks the device tree with a bounded depth.
func collectServices(device *upnpDevice, depth int, out *[]upnpService) {
	if depth > maxDeviceDepth {
		return
	}
	*out = append(*out, device.Services...)
	for i := range device.Devices {
		collectServices(&device.Devices[i], depth+1, out)
	}
}

// controlTargets parses a device description and returns the control
// endpoints that are hosted on gateway, best service type first.
func controlTargets(body []byte, location *url.URL, gateway netip.Addr) []upnpTarget {
	var root upnpRoot
	if err := xml.Unmarshal(body, &root); err != nil {
		return nil
	}

	base := location
	if trimmed := strings.TrimSpace(root.URLBase); trimmed != "" {
		if parsed, err := url.Parse(trimmed); err == nil && parsed.IsAbs() {
			base = parsed
		}
	}
	if addr, ok := hostAddr(base); !ok || addr != gateway {
		return nil
	}

	var services []upnpService
	collectServices(&root.Device, 0, &services)

	var targets []upnpTarget
	for _, service := range services {
		rank := slices.Index(upnpServiceTypes, strings.TrimSpace(service.ServiceType))
		if rank < 0 {
			continue
		}
		control, ok := resolveControlURL(base, service.ControlURL, gateway)
		if !ok {
			continue
		}
		targets = append(targets, upnpTarget{gateway: gateway, control: control, service: strings.TrimSpace(service.ServiceType)})
	}
	slices.SortStableFunc(targets, func(a, b upnpTarget) int {
		return slices.Index(upnpServiceTypes, a.service) - slices.Index(upnpServiceTypes, b.service)
	})
	return targets
}

// resolveControlURL resolves a router-supplied control URL against the
// description base and refuses anything that leaves the gateway host.
func resolveControlURL(base *url.URL, raw string, gateway netip.Addr) (*url.URL, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}
	resolved := base.ResolveReference(ref)
	if resolved.Scheme != "http" {
		return nil, false
	}
	addr, ok := hostAddr(resolved)
	if !ok || addr != gateway {
		return nil, false
	}
	resolved.User = nil
	return resolved, true
}

func (c *client) discoverUPnP(ctx context.Context, preferred []netip.Addr) ([]upnpTarget, error) {
	locations, err := c.ssdpSearch(ctx)
	if err != nil {
		return nil, err
	}

	gateways := make([]netip.Addr, 0, len(locations))
	for gateway := range locations {
		gateways = append(gateways, gateway)
	}
	// Try the host that also routes our traffic first.
	slices.SortStableFunc(gateways, func(a, b netip.Addr) int {
		return preferenceRank(a, preferred) - preferenceRank(b, preferred)
	})

	var targets []upnpTarget
	for _, gateway := range gateways {
		if ctx.Err() != nil {
			break
		}
		body, err := c.fetchDescription(ctx, locations[gateway])
		if err != nil {
			c.logger.Debug("fetching upnp description failed", "gateway", gateway, "error", err)
			continue
		}
		targets = append(targets, controlTargets(body, locations[gateway], gateway)...)
	}
	if len(targets) == 0 {
		return nil, errors.New("no usable igd control endpoint")
	}
	return targets, nil
}

func preferenceRank(addr netip.Addr, preferred []netip.Addr) int {
	if slices.Contains(preferred, addr) {
		return 0
	}
	return 1
}

func (c *client) fetchDescription(ctx context.Context, location *url.URL) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("description http status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDescriptionBytes))
}

// soapArg is one AddPortMapping-style argument.
type soapArg struct {
	Name  string
	Value string
}

// soapBody renders a SOAP request body for a UPnP action.
func soapBody(service, action string, args []soapArg) []byte {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0"?>` + "\n")
	buf.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">`)
	buf.WriteString(`<s:Body>`)
	buf.WriteString(`<u:` + action + ` xmlns:u="` + service + `">`)
	for _, arg := range args {
		buf.WriteString(`<` + arg.Name + `>`)
		xml.EscapeText(&buf, []byte(arg.Value))
		buf.WriteString(`</` + arg.Name + `>`)
	}
	buf.WriteString(`</u:` + action + `>`)
	buf.WriteString(`</s:Body></s:Envelope>`)
	return buf.Bytes()
}

// soapValue returns the text of the first element with the given local name.
func soapValue(body []byte, name string) (string, bool) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", false
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != name {
			continue
		}
		var value string
		if err := decoder.DecodeElement(&value, &start); err != nil {
			return "", false
		}
		return strings.TrimSpace(value), true
	}
}

// soapFault turns an error response body into a typed error.
func soapFault(status int, body []byte) *upnpError {
	fault := &upnpError{Status: status}
	if raw, ok := soapValue(body, "errorCode"); ok {
		if code, err := strconv.Atoi(raw); err == nil {
			fault.Code = code
		}
	}
	if description, ok := soapValue(body, "errorDescription"); ok {
		fault.Description = description
	}
	return fault
}

func (c *client) soapCall(ctx context.Context, target upnpTarget, action string, args []soapArg) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()

	body := soapBody(target.service, action, args)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.control.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+target.service+"#"+action+`"`)
	req.Header.Set("Connection", "close")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxSOAPBytes))
	if resp.StatusCode != http.StatusOK {
		return nil, soapFault(resp.StatusCode, data)
	}
	if readErr != nil {
		return nil, fmt.Errorf("read soap response: %w", readErr)
	}
	return data, nil
}

func addPortMappingArgs(externalPort, internalPort uint16, client netip.Addr, lease uint32) []soapArg {
	return []soapArg{
		{Name: "NewRemoteHost", Value: ""},
		{Name: "NewExternalPort", Value: strconv.FormatUint(uint64(externalPort), 10)},
		{Name: "NewProtocol", Value: "UDP"},
		{Name: "NewInternalPort", Value: strconv.FormatUint(uint64(internalPort), 10)},
		{Name: "NewInternalClient", Value: client.String()},
		{Name: "NewEnabled", Value: "1"},
		{Name: "NewPortMappingDescription", Value: upnpDescription},
		{Name: "NewLeaseDuration", Value: strconv.FormatUint(uint64(lease), 10)},
	}
}

func (c *client) mapUPnP(ctx context.Context, preferred []netip.Addr, internalPort uint16, lifetime time.Duration) (*Mapping, error) {
	targets, err := c.discoverUPnP(ctx, preferred)
	if err != nil {
		return nil, err
	}

	var failures []error
	for _, target := range targets {
		if ctx.Err() != nil {
			break
		}
		mapping, err := c.addUPnPMapping(ctx, target, internalPort, lifetime)
		if err == nil {
			return mapping, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", target.service, err))
	}
	if len(failures) == 0 {
		return nil, errors.New("no igd control endpoint accepted a mapping")
	}
	return nil, errors.Join(failures...)
}

func (c *client) addUPnPMapping(ctx context.Context, target upnpTarget, internalPort uint16, lifetime time.Duration) (*Mapping, error) {
	local, err := c.localAddrFor(target.gateway)
	if err != nil {
		return nil, err
	}

	externalPort := internalPort
	lease := uint32(lifetime / time.Second)
	granted := lifetime

	for attempt := 0; ; attempt++ {
		_, err = c.soapCall(ctx, target, "AddPortMapping", addPortMappingArgs(externalPort, internalPort, local, lease))
		if err == nil {
			break
		}

		var fault *upnpError
		if !errors.As(err, &fault) || attempt > upnpConflictRetryPortAttempt {
			return nil, err
		}
		switch fault.Code {
		case upnpErrOnlyPermanentLeases:
			// The router only does permanent mappings; keep our own clock.
			lease = 0
		case upnpErrConflictingMapping:
			externalPort = randomEphemeralPort()
		default:
			return nil, err
		}
	}

	external := netip.IPv4Unspecified()
	if addr, err := c.upnpExternalAddr(ctx, target); err != nil {
		c.logger.Debug("upnp external address unavailable", "error", err)
	} else {
		external = addr
	}

	return &Mapping{
		External:  netip.AddrPortFrom(external, externalPort),
		Internal:  internalPort,
		Method:    MethodUPnP,
		Lifetime:  granted,
		ExpiresAt: time.Now().Add(granted),
		owner:     c,
		gateway:   target.gateway,
		local:     local,
		control:   target.control.String(),
		service:   target.service,
	}, nil
}

func (c *client) upnpExternalAddr(ctx context.Context, target upnpTarget) (netip.Addr, error) {
	body, err := c.soapCall(ctx, target, "GetExternalIPAddress", nil)
	if err != nil {
		return netip.Addr{}, err
	}
	raw, ok := soapValue(body, "NewExternalIPAddress")
	if !ok {
		return netip.Addr{}, errors.New("external address missing from response")
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("external address %q malformed", raw)
	}
	return addr.Unmap(), nil
}

func (c *client) releaseUPnP(ctx context.Context, m *Mapping) error {
	control, err := url.Parse(m.control)
	if err != nil {
		return fmt.Errorf("parse control url: %w", err)
	}
	if addr, ok := hostAddr(control); !ok || addr != m.gateway {
		return errors.New("control url no longer points at the gateway")
	}

	target := upnpTarget{gateway: m.gateway, control: control, service: m.service}
	_, err = c.soapCall(ctx, target, "DeletePortMapping", []soapArg{
		{Name: "NewRemoteHost", Value: ""},
		{Name: "NewExternalPort", Value: strconv.FormatUint(uint64(m.External.Port()), 10)},
		{Name: "NewProtocol", Value: "UDP"},
	})
	return err
}

// randomEphemeralPort picks a replacement external port after a conflict.
func randomEphemeralPort() uint16 {
	var buf [2]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return upnpEphemeralPortBase
	}
	return uint16(upnpEphemeralPortBase + int(binary.BigEndian.Uint16(buf[:]))%upnpEphemeralPortCount)
}
