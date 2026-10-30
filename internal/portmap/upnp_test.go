package portmap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSoapBody(t *testing.T) {
	const service = "urn:schemas-upnp-org:service:WANIPConnection:1"
	got := string(soapBody(service, "AddPortMapping", addPortMappingArgs(50000, 41234, netip.MustParseAddr("192.168.1.34"), 3600)))

	want := `<?xml version="1.0"?>` + "\n" +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body>` +
		`<u:AddPortMapping xmlns:u="` + service + `">` +
		`<NewRemoteHost></NewRemoteHost>` +
		`<NewExternalPort>50000</NewExternalPort>` +
		`<NewProtocol>UDP</NewProtocol>` +
		`<NewInternalPort>41234</NewInternalPort>` +
		`<NewInternalClient>192.168.1.34</NewInternalClient>` +
		`<NewEnabled>1</NewEnabled>` +
		`<NewPortMappingDescription>herdr-mobile-relay</NewPortMappingDescription>` +
		`<NewLeaseDuration>3600</NewLeaseDuration>` +
		`</u:AddPortMapping>` +
		`</s:Body></s:Envelope>`
	if got != want {
		t.Fatalf("soapBody =\n%s\nwant\n%s", got, want)
	}
}

func TestSoapBodyEscapesValues(t *testing.T) {
	got := string(soapBody("svc", "Action", []soapArg{{Name: "NewRemoteHost", Value: `<evil>&"`}}))
	if strings.Contains(got, "<evil>") {
		t.Fatalf("argument value was not escaped: %s", got)
	}
	if !strings.Contains(got, "&lt;evil&gt;") {
		t.Fatalf("expected escaped value, got %s", got)
	}
}

func TestSoapValue(t *testing.T) {
	body := []byte(`<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>
<u:GetExternalIPAddressResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1">
<NewExternalIPAddress>203.0.113.11</NewExternalIPAddress>
</u:GetExternalIPAddressResponse></s:Body></s:Envelope>`)

	value, ok := soapValue(body, "NewExternalIPAddress")
	if !ok || value != "203.0.113.11" {
		t.Fatalf("soapValue = %q, %v", value, ok)
	}
	if _, ok := soapValue(body, "NewMissing"); ok {
		t.Fatal("soapValue found an absent element")
	}
	if _, ok := soapValue([]byte("<not xml"), "NewExternalIPAddress"); ok {
		t.Fatal("soapValue accepted malformed xml")
	}
}

func TestSoapFault(t *testing.T) {
	body := []byte(`<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault>
<faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring>
<detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0">
<errorCode>725</errorCode><errorDescription>OnlyPermanentLeasesSupported</errorDescription>
</UPnPError></detail></s:Fault></s:Body></s:Envelope>`)

	fault := soapFault(http.StatusInternalServerError, body)
	if fault.Code != upnpErrOnlyPermanentLeases {
		t.Fatalf("code = %d, want %d", fault.Code, upnpErrOnlyPermanentLeases)
	}
	if fault.Description != "OnlyPermanentLeasesSupported" {
		t.Fatalf("description = %q", fault.Description)
	}
	if fault.Error() == "" {
		t.Fatal("empty error text")
	}

	plain := soapFault(http.StatusNotFound, []byte("nope"))
	if plain.Code != 0 || plain.Status != http.StatusNotFound {
		t.Fatalf("plain fault = %+v", plain)
	}
}

const igdDescription = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
    <deviceList>
      <device>
        <deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
        <deviceList>
          <device>
            <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
            <serviceList>
              <service>
                <serviceType>urn:schemas-upnp-org:service:WANPPPConnection:1</serviceType>
                <controlURL>/ctl/PPPConn</controlURL>
              </service>
              <service>
                <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
                <controlURL>/ctl/IPConn</controlURL>
              </service>
            </serviceList>
          </device>
        </deviceList>
      </device>
    </deviceList>
  </device>
</root>`

func TestControlTargets(t *testing.T) {
	location, err := url.Parse("http://192.168.1.1:5000/rootDesc.xml")
	if err != nil {
		t.Fatal(err)
	}
	gateway := netip.MustParseAddr("192.168.1.1")

	targets := controlTargets([]byte(igdDescription), location, gateway)
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(targets))
	}
	// WANIPConnection outranks WANPPPConnection.
	if targets[0].service != "urn:schemas-upnp-org:service:WANIPConnection:1" {
		t.Errorf("first service = %s", targets[0].service)
	}
	if got := targets[0].control.String(); got != "http://192.168.1.1:5000/ctl/IPConn" {
		t.Errorf("first control url = %s", got)
	}
	if targets[1].service != "urn:schemas-upnp-org:service:WANPPPConnection:1" {
		t.Errorf("second service = %s", targets[1].service)
	}
}

func TestControlTargetsRejectsForeignHosts(t *testing.T) {
	location, err := url.Parse("http://192.168.1.1:5000/rootDesc.xml")
	if err != nil {
		t.Fatal(err)
	}
	gateway := netip.MustParseAddr("192.168.1.1")

	tests := []struct {
		name        string
		description string
	}{
		{
			name:        "absolute control url on another host",
			description: strings.Replace(igdDescription, "<controlURL>/ctl/IPConn</controlURL>", "<controlURL>http://169.254.169.254/latest/meta-data</controlURL>", 1),
		},
		{
			name:        "control url with a hostname",
			description: strings.Replace(igdDescription, "<controlURL>/ctl/IPConn</controlURL>", "<controlURL>http://attacker.example.com/ctl</controlURL>", 1),
		},
		{
			name:        "urlbase moves the whole device off the gateway",
			description: strings.Replace(igdDescription, "<device>", "<URLBase>http://203.0.113.5:80/</URLBase><device>", 1),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, target := range controlTargets([]byte(tc.description), location, gateway) {
				if addr, ok := hostAddr(target.control); !ok || addr != gateway {
					t.Fatalf("accepted off-gateway control url %s", target.control)
				}
			}
		})
	}

	// The malformed-XML case must not panic and must yield nothing.
	if targets := controlTargets([]byte("<root"), location, gateway); targets != nil {
		t.Fatalf("malformed description produced %d targets", len(targets))
	}
}

func TestResolveControlURL(t *testing.T) {
	base, err := url.Parse("http://192.168.1.1:5000/rootDesc.xml")
	if err != nil {
		t.Fatal(err)
	}
	gateway := netip.MustParseAddr("192.168.1.1")

	if got, ok := resolveControlURL(base, "/ctl/IPConn", gateway); !ok || got.String() != "http://192.168.1.1:5000/ctl/IPConn" {
		t.Fatalf("relative control url = %v, %v", got, ok)
	}
	for _, raw := range []string{
		"",
		"http://203.0.113.5/ctl",
		"http://router.local/ctl",
		"https://192.168.1.1:5000/ctl",
		"file:///etc/passwd",
		"http://user:pass@203.0.113.5/ctl",
	} {
		if got, ok := resolveControlURL(base, raw, gateway); ok {
			t.Errorf("resolveControlURL(%q) accepted %s", raw, got)
		}
	}
}

func TestParseSSDPLocation(t *testing.T) {
	ok := []byte("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=120\r\nLOCATION: http://192.168.1.1:5000/rootDesc.xml\r\nST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n\r\n")
	location, found := parseSSDPLocation(ok)
	if !found || location.String() != "http://192.168.1.1:5000/rootDesc.xml" {
		t.Fatalf("parseSSDPLocation = %v, %v", location, found)
	}

	for _, datagram := range [][]byte{
		nil,
		[]byte("garbage"),
		[]byte("HTTP/1.1 404 Not Found\r\nLOCATION: http://192.168.1.1/x\r\n\r\n"),
		[]byte("HTTP/1.1 200 OK\r\nST: x\r\n\r\n"),
		[]byte("HTTP/1.1 200 OK\r\nLOCATION: https://192.168.1.1/x\r\n\r\n"),
		[]byte("HTTP/1.1 200 OK\r\nLOCATION: ::::\r\n\r\n"),
	} {
		if _, found := parseSSDPLocation(datagram); found {
			t.Errorf("parseSSDPLocation accepted %q", datagram)
		}
	}
}

func TestRouterAddrAllowed(t *testing.T) {
	allowed := []string{"192.168.1.1", "10.0.0.1", "172.16.0.1", "127.0.0.1", "169.254.1.1", "fe80::1", "fd00::1"}
	for _, raw := range allowed {
		if !routerAddrAllowed(netip.MustParseAddr(raw)) {
			t.Errorf("routerAddrAllowed(%s) = false", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "203.0.113.5", "2001:4860:4860::8888"} {
		if routerAddrAllowed(netip.MustParseAddr(raw)) {
			t.Errorf("routerAddrAllowed(%s) = true", raw)
		}
	}
	if routerAddrAllowed(netip.Addr{}) {
		t.Error("routerAddrAllowed(invalid) = true")
	}
}

// fakeIGD is an in-process UPnP control endpoint on loopback.
type fakeIGD struct {
	mu       sync.Mutex
	actions  []string
	leases   []string
	deleted  []string
	rejector func(action string, call int) (int, string)
}

func (f *fakeIGD) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxSOAPBytes))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		action := strings.Trim(r.Header.Get("SOAPAction"), `"`)
		if idx := strings.Index(action, "#"); idx >= 0 {
			action = action[idx+1:]
		}

		f.mu.Lock()
		f.actions = append(f.actions, action)
		call := 0
		for _, seen := range f.actions {
			if seen == action {
				call++
			}
		}
		switch action {
		case "AddPortMapping":
			lease, _ := soapValue(body, "NewLeaseDuration")
			f.leases = append(f.leases, lease)
		case "DeletePortMapping":
			port, _ := soapValue(body, "NewExternalPort")
			f.deleted = append(f.deleted, port)
		}
		rejector := f.rejector
		f.mu.Unlock()

		if rejector != nil {
			if status, fault := rejector(action, call); status != 0 {
				w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
				w.WriteHeader(status)
				io.WriteString(w, fault)
				return
			}
		}

		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		switch action {
		case "GetExternalIPAddress":
			io.WriteString(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>`+
				`<u:GetExternalIPAddressResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1">`+
				`<NewExternalIPAddress>203.0.113.11</NewExternalIPAddress></u:GetExternalIPAddressResponse></s:Body></s:Envelope>`)
		default:
			io.WriteString(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>`+
				`<u:`+action+`Response xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"/></s:Body></s:Envelope>`)
		}
	}
}

func (f *fakeIGD) snapshot() ([]string, []string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actions...), append([]string(nil), f.leases...), append([]string(nil), f.deleted...)
}

const permanentLeaseFault = `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault>` +
	`<faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0">` +
	`<errorCode>725</errorCode><errorDescription>OnlyPermanentLeasesSupported</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`

func TestAddUPnPMappingRetriesPermanentLeaseAndReleases(t *testing.T) {
	igd := &fakeIGD{
		rejector: func(action string, call int) (int, string) {
			if action == "AddPortMapping" && call == 1 {
				return http.StatusInternalServerError, permanentLeaseFault
			}
			return 0, ""
		},
	}
	server := httptest.NewServer(igd.handler(t))
	defer server.Close()

	control, err := url.Parse(server.URL + "/ctl/IPConn")
	if err != nil {
		t.Fatal(err)
	}
	gateway, ok := hostAddr(control)
	if !ok {
		t.Fatalf("test server host %s is not an ip literal", control.Host)
	}

	c := newClient(testLogger())
	c.httpClient = server.Client()
	target := upnpTarget{gateway: gateway, control: control, service: upnpServiceTypes[1]}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mapping, err := c.addUPnPMapping(ctx, target, 41234, time.Hour)
	if err != nil {
		t.Fatalf("addUPnPMapping: %v", err)
	}
	if mapping.Method != MethodUPnP {
		t.Errorf("method = %s, want %s", mapping.Method, MethodUPnP)
	}
	if mapping.Internal != 41234 || mapping.External.Port() != 41234 {
		t.Errorf("ports = %d/%d, want 41234/41234", mapping.Internal, mapping.External.Port())
	}
	if got := mapping.External.Addr().String(); got != "203.0.113.11" {
		t.Errorf("external ip = %s, want 203.0.113.11", got)
	}

	actions, leases, _ := igd.snapshot()
	if len(actions) != 3 || actions[0] != "AddPortMapping" || actions[1] != "AddPortMapping" || actions[2] != "GetExternalIPAddress" {
		t.Fatalf("actions = %v", actions)
	}
	if leases[0] != "3600" || leases[1] != "0" {
		t.Fatalf("lease durations = %v, want [3600 0]", leases)
	}

	if err := mapping.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	_, _, deleted := igd.snapshot()
	if len(deleted) != 1 || deleted[0] != "41234" {
		t.Fatalf("deleted external ports = %v", deleted)
	}
}

func TestReleaseUPnPRejectsOffGatewayControlURL(t *testing.T) {
	c := newClient(testLogger())
	mapping := &Mapping{
		Method:   MethodUPnP,
		owner:    c,
		gateway:  netip.MustParseAddr("192.168.1.1"),
		control:  "http://203.0.113.5/ctl",
		service:  upnpServiceTypes[1],
		External: netip.MustParseAddrPort("203.0.113.11:41234"),
	}
	if err := mapping.Release(context.Background()); err == nil {
		t.Fatal("Release followed a control url that left the gateway")
	}
}

// startFakeSSDP answers M-SEARCH datagrams on loopback with a fixed LOCATION.
func startFakeSSDP(t *testing.T, location string) string {
	t.Helper()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen fake ssdp: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, ssdpMaxDatagram)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if !strings.HasPrefix(string(buf[:n]), "M-SEARCH") {
				continue
			}
			conn.WriteToUDP([]byte("HTTP/1.1 200 OK\r\n"+
				"CACHE-CONTROL: max-age=120\r\n"+
				"ST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n"+
				"USN: uuid:fake::urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n"+
				"LOCATION: "+location+"\r\n\r\n"), from)
		}
	}()
	t.Cleanup(func() {
		conn.Close()
		<-done
	})
	return conn.LocalAddr().String()
}

func TestMapUPnPDiscoversOverSSDP(t *testing.T) {
	igd := &fakeIGD{}
	control := igd.handler(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/rootDesc.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		io.WriteString(w, igdDescription)
	})
	mux.Handle("/ctl/IPConn", control)
	mux.Handle("/ctl/PPPConn", control)
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newClient(testLogger())
	c.httpClient = server.Client()
	c.ssdpAddr = startFakeSSDP(t, server.URL+"/rootDesc.xml")
	c.ssdpWait = 300 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mapping, err := c.mapUPnP(ctx, nil, 41234, time.Hour)
	if err != nil {
		t.Fatalf("mapUPnP: %v", err)
	}
	if mapping.Method != MethodUPnP {
		t.Errorf("method = %s, want %s", mapping.Method, MethodUPnP)
	}
	if got, want := mapping.External, netip.MustParseAddrPort("203.0.113.11:41234"); got != want {
		t.Errorf("external = %s, want %s", got, want)
	}
	if !strings.HasSuffix(mapping.control, "/ctl/IPConn") {
		t.Errorf("control url = %s, want the WANIPConnection endpoint", mapping.control)
	}
	if mapping.Lifetime != time.Hour {
		t.Errorf("lifetime = %s, want 1h", mapping.Lifetime)
	}

	actions, leases, _ := igd.snapshot()
	if len(actions) != 2 || actions[0] != "AddPortMapping" || actions[1] != "GetExternalIPAddress" {
		t.Fatalf("actions = %v", actions)
	}
	if leases[0] != "3600" {
		t.Errorf("lease = %s, want 3600", leases[0])
	}

	if err := mapping.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, _, deleted := igd.snapshot(); len(deleted) != 1 || deleted[0] != "41234" {
		t.Fatalf("deleted = %v", deleted)
	}
}

func TestSSDPSearchIgnoresOffHostLocations(t *testing.T) {
	c := newClient(testLogger())
	c.ssdpAddr = startFakeSSDP(t, "http://203.0.113.5:80/rootDesc.xml")
	c.ssdpWait = 300 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if found, err := c.ssdpSearch(ctx); err == nil {
		t.Fatalf("ssdpSearch accepted a foreign location: %v", found)
	}
}

var errUPnPRedirectDial = errors.New("guard blocked forbidden UPnP redirect dial")

type upnpRedirectDialGuard struct {
	mu     sync.Mutex
	denied []string
}

func (g *upnpRedirectDialGuard) snapshot() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.denied...)
}

func upnpRedirectServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func guardedUPnPRedirectClient(t *testing.T, servers ...*httptest.Server) (*client, *upnpRedirectDialGuard) {
	t.Helper()
	c := newClient(testLogger())
	transport := c.httpClient.Transport.(*http.Transport)
	t.Cleanup(transport.CloseIdleConnections)
	originalDial := transport.DialContext
	allowed := make(map[string]bool)
	for _, server := range servers {
		endpoint := server.Listener.Addr().String()
		addr, err := netip.ParseAddrPort(endpoint)
		if err != nil || !addr.Addr().IsLoopback() || addr.Port() == 0 {
			t.Fatalf("non-numeric/non-loopback owned endpoint: %q", endpoint)
		}
		allowed[endpoint] = true
	}
	guard := &upnpRedirectDialGuard{}
	// Keep the production client/transport; only exact owned numeric endpoints
	// may reach its original dialer. Denials occur before DNS or TLS work.
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		guard.mu.Lock()
		ok := allowed[address]
		if !ok {
			guard.denied = append(guard.denied, address)
		}
		guard.mu.Unlock()
		if !ok {
			return nil, errUPnPRedirectDial
		}
		return originalDial(ctx, network, address)
	}
	return c, guard
}

func callUPnPRedirect(ctx context.Context, c *client, operation, endpoint string) ([]byte, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if operation == "description" {
		return c.fetchDescription(ctx, u)
	}
	gateway, _ := hostAddr(u)
	return c.soapCall(ctx, upnpTarget{gateway: gateway, control: u, service: upnpServiceTypes[1]}, "GetExternalIPAddress", nil)
}

func TestUPnPRedirectAllowed(t *testing.T) {
	for _, operation := range []string{"description", "soap"} {
		for _, tc := range []struct {
			status int
			kind   string
		}{
			{301, "relative"}, {301, "same_ip_port"},
			{302, "relative"}, {302, "same_ip_port"},
			{303, "relative"}, {303, "same_ip_port"},
			{307, "relative"}, {307, "same_ip_port"},
			{308, "relative"}, {308, "same_ip_port"},
			{302, "drop_then_307"},
		} {
			t.Run(fmt.Sprintf("%s/%d/%s", operation, tc.status, tc.kind), func(t *testing.T) {
				type observation struct {
					method, body string
					err          error
				}
				seen := make(chan observation, 12)
				const payload = "<redirect>ok</redirect>"
				var destination string
				handler := func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					seen <- observation{r.Method, string(body), err}
					switch {
					case r.URL.Path == "/start":
						http.Redirect(w, r, destination, tc.status)
					case r.URL.Path == "/next" && tc.kind == "drop_then_307":
						http.Redirect(w, r, "/end", http.StatusTemporaryRedirect)
					default:
						io.WriteString(w, payload)
					}
				}
				start := upnpRedirectServer(t, handler)
				end := upnpRedirectServer(t, handler)
				destination = "/next"
				if tc.kind == "same_ip_port" {
					destination = end.URL + "/next"
				}
				c, guard := guardedUPnPRedirectClient(t, start, end)
				body, err := callUPnPRedirect(context.Background(), c, operation, start.URL+"/start")
				if err != nil || string(body) != payload {
					t.Fatalf("response = %q, %v", body, err)
				}
				wantCount := 2
				if tc.kind == "drop_then_307" {
					wantCount = 3
				}
				if len(seen) != wantCount {
					t.Fatalf("requests = %d, want %d", len(seen), wantCount)
				}
				method, requestBody := http.MethodGet, ""
				if operation == "soap" {
					method, requestBody = http.MethodPost, string(soapBody(upnpServiceTypes[1], "GetExternalIPAddress", nil))
				}
				for i := 0; i < wantCount; i++ {
					if i > 0 && tc.status < 307 {
						method, requestBody = http.MethodGet, ""
					}
					if got := <-seen; got.err != nil || got.method != method || got.body != requestBody {
						t.Fatalf("hop %d = %+v, want %s %q", i, got, method, requestBody)
					}
				}
				if denied := guard.snapshot(); len(denied) != 0 {
					t.Fatalf("unexpected dial denials: %v", denied)
				}
			})
		}
	}
}

func TestUPnPRedirectChainLimit(t *testing.T) {
	for _, operation := range []string{"description", "soap"} {
		t.Run(operation, func(t *testing.T) {
			var requests atomic.Int32
			server := upnpRedirectServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Redirect(w, r, "/loop", http.StatusTemporaryRedirect)
			})
			c, guard := guardedUPnPRedirectClient(t, server)
			_, err := callUPnPRedirect(context.Background(), c, operation, server.URL)
			var urlErr *url.Error
			if !errors.As(err, &urlErr) || urlErr.Err.Error() != "stopped after 10 redirects" {
				t.Fatalf("chain error = %v, want Go's ten-redirect cause", err)
			}
			if got := requests.Load(); got != 10 {
				t.Fatalf("server requests = %d, want exactly 10 (not 11)", got)
			}
			if denied := guard.snapshot(); len(denied) != 0 {
				t.Fatalf("unexpected dial denials: %v", denied)
			}
		})
	}
}

func TestUPnPRedirectCancellation(t *testing.T) {
	for _, operation := range []string{"description", "soap"} {
		t.Run(operation, func(t *testing.T) {
			watchdog, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			ctx, cancel := context.WithCancel(watchdog)
			defer cancel()
			entered := make(chan struct{}, 1)
			end := upnpRedirectServer(t, func(w http.ResponseWriter, r *http.Request) {
				entered <- struct{}{}
				select {
				case <-r.Context().Done():
				case <-watchdog.Done():
				}
			})
			start := upnpRedirectServer(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, end.URL, http.StatusTemporaryRedirect)
			})
			c, guard := guardedUPnPRedirectClient(t, start, end)
			cancelled := make(chan bool, 1)
			go func() {
				select {
				case <-entered:
					cancel()
					cancelled <- true
				case <-watchdog.Done():
					cancelled <- false
				}
			}()
			_, err := callUPnPRedirect(ctx, c, operation, start.URL)
			stop()
			if didEnter := <-cancelled; !didEnter || !errors.Is(err, context.Canceled) {
				t.Fatalf("redirected handler entered=%t, error=%v, want context.Canceled", didEnter, err)
			}
			if denied := guard.snapshot(); len(denied) != 0 {
				t.Fatalf("unexpected dial denials: %v", denied)
			}
		})
	}
}

func TestUPnPRedirectRejectsEscape(t *testing.T) {
	for _, op := range []struct {
		name, operation string
		status          int
	}{{"description_302", "description", 302}, {"soap_307", "soap", 307}} {
		t.Run(op.name, func(t *testing.T) {
			for _, dest := range []struct {
				name, endpoint, dial string
			}{
				{"off_ip", "http://203.0.113.9:80/escape", "203.0.113.9:80"},
				{"hostname", "http://escape.invalid:80/escape", "escape.invalid:80"},
				{"https", "https://127.0.0.1:1/escape", "127.0.0.1:1"},
			} {
				t.Run(dest.name, func(t *testing.T) {
					for _, position := range []string{"direct", "after_same_ip_port"} {
						t.Run(position, func(t *testing.T) {
							var requests atomic.Int32
							intermediate := upnpRedirectServer(t, func(w http.ResponseWriter, r *http.Request) {
								requests.Add(1)
								http.Redirect(w, r, dest.endpoint, op.status)
							})
							destination, wantRequests := dest.endpoint, int32(1)
							if position == "after_same_ip_port" {
								destination, wantRequests = intermediate.URL, 2
							}
							start := upnpRedirectServer(t, func(w http.ResponseWriter, r *http.Request) {
								requests.Add(1)
								http.Redirect(w, r, destination, op.status)
							})
							c, guard := guardedUPnPRedirectClient(t, start, intermediate)
							_, err := callUPnPRedirect(context.Background(), c, op.operation, start.URL)
							var urlErr *url.Error
							if !errors.As(err, &urlErr) || urlErr.Timeout() || requests.Load() != wantRequests {
								t.Fatalf("requests=%d, error=%v, want %d owned requests and redirect error", requests.Load(), err, wantRequests)
							}
							denied, sentinel := guard.snapshot(), errors.Is(err, errUPnPRedirectDial)
							t.Logf("guard denied=%d sentinel=%t destinations=%v error=%v", len(denied), sentinel, denied, err)
							if len(denied) > 0 && (len(denied) != 1 || denied[0] != dest.dial || !sentinel) {
								t.Fatalf("unexpected guard outcome: denied=%v sentinel=%t", denied, sentinel)
							}
							if len(denied) != 0 {
								t.Fatalf("forbidden dial attempts: got %d, want 0", len(denied))
							}
						})
					}
				})
			}
		})
	}
}
