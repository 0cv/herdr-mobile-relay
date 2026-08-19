package portmap

import (
	"context"
	"net/netip"
	"strings"
	"testing"
)

const procNetRouteSample = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
wlan0	00000000	0101A8C0	0003	0	0	600	00000000	0	0	0
wlan0	0001A8C0	00000000	0001	0	0	600	00FFFFFF	0	0	0
eth0	00000000	FE01A8C0	0003	0	0	100	00000000	0	0	0
eth0	00000000	0101A8C0	0003	0	0	900	00000000	0	0	0
docker0	000011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
tun0	00000000	00000000	0001	0	0	50	00000000	0	0	0
`

func TestParseProcNetRoute(t *testing.T) {
	got := parseProcNetRoute(strings.NewReader(procNetRouteSample))
	want := []netip.Addr{
		netip.MustParseAddr("192.168.1.1"),
		netip.MustParseAddr("192.168.1.254"),
	}
	if len(got) != len(want) {
		t.Fatalf("gateways = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("gateways = %v, want %v", got, want)
		}
	}
}

func TestParseProcNetRouteHostileInput(t *testing.T) {
	inputs := []string{
		"",
		"garbage\n",
		"iface\n",
		"wlan0	00000000\n",
		"wlan0	00000000	ZZZZZZZZ	0003	0\n",
		"wlan0	00000000	0101A8C	0003	0\n",
		"wlan0	00000000	0101A8C0	NOTHEX	0\n",
		"wlan0	00000000	0101A8C0	0001	0\n", // no RTF_GATEWAY
		strings.Repeat("x\n", 20000),
	}
	for _, input := range inputs {
		if got := parseProcNetRoute(strings.NewReader(input)); len(got) != 0 {
			t.Errorf("parseProcNetRoute(%q) = %v, want none", input, got)
		}
	}
}

func TestParseRouteGet(t *testing.T) {
	out := `   route to: default
destination: default
       mask: default
    gateway: 192.168.7.1
  interface: en0
      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING,GLOBAL>
`
	got := parseRouteGet(out)
	if len(got) != 1 || got[0] != netip.MustParseAddr("192.168.7.1") {
		t.Fatalf("parseRouteGet = %v, want [192.168.7.1]", got)
	}

	for _, bad := range []string{"", "no gateway here", "gateway: not-an-ip", "gateway: 0.0.0.0", "gatewayish: 1.2.3.4"} {
		if got := parseRouteGet(bad); len(got) != 0 {
			t.Errorf("parseRouteGet(%q) = %v, want none", bad, got)
		}
	}
}

func TestDefaultGatewaysDoesNotFail(t *testing.T) {
	// The host may or may not have a default route; discovery must never
	// panic or block, and an error is a legitimate outcome.
	gateways, err := defaultGateways(context.Background())
	if err != nil {
		t.Logf("defaultGateways: %v", err)
		return
	}
	for _, gateway := range gateways {
		if !gateway.IsValid() {
			t.Fatalf("invalid gateway in %v", gateways)
		}
	}
}
