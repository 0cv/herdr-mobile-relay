package portmap

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
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
