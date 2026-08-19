package blackbox

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// natMatrixEnvVar guards the whole NAT matrix. The harness needs root, Linux
// network namespaces and nftables, none of which a plain `go test
// ./tests/blackbox/` run can assume, so it stays opt-in behind one variable and
// `make nat-matrix`.
const natMatrixEnvVar = "HERDR_NAT_MATRIX"

// The matrix wires five network namespaces into two homes behind their own NAT
// with the internet between them:
//
//	relay ── relay NAT ──┐              ┌── phone NAT ── phone
//	                     └── internet ──┘
//	                     (blind gateway)
//
// Every address is documentation or private space and lives only inside those
// namespaces. The harness adds no interface, address, route or rule to the
// namespace it was started from, so the worst a killed run can leave behind is
// five namespaces named after its own random suffix.
const (
	// natGatewayAddr is the blind gateway's service address. It sits on lo in
	// the internet namespace rather than on one of the two backbone links,
	// because a socket bound to a link address would answer the far home from
	// the wrong source address and break every flow through it.
	natGatewayAddr = "203.0.113.9"

	// The two point-to-point backbone links. Separate /30s mean the internet
	// namespace routes between the homes without a bridge.
	natInetRelaySide = "198.51.100.1"
	natRelayExtAddr  = "198.51.100.2"
	natInetPhoneSide = "198.51.100.5"
	natPhoneExtAddr  = "198.51.100.6"

	// The two home LANs. Neither is routable from the other home or from the
	// internet namespace, so a host candidate is never a reachable target for the
	// peer and only a mapped address can carry a direct pair — which is the
	// situation the matrix is about.
	natRelayGatewayAddr = "10.77.1.1"
	natRelayHostAddr    = "10.77.1.2"
	natPhoneGatewayAddr = "10.77.2.1"
	natPhoneHostAddr    = "10.77.2.2"
)

// The behaviour names are the RFC 4787 terms, and they double as the values in
// the cell plan the phone namespace receives and as the answer its NAT
// behaviour probe reports.
const (
	natMappingIndependent = "endpoint-independent"
	natMappingSymmetric   = "endpoint-dependent"
	natFilterAddress      = "address-dependent"
	natFilterAddressPort  = "address-and-port-dependent"
)

// natCell is one square of the matrix: how the NAT allocates an external port,
// how it treats an unsolicited inbound datagram, and whether a direct pair is
// expected to form through it.
type natCell struct {
	name      string
	mapping   string
	filtering string
	direct    bool
}

// requireNATMatrixHost skips — never fails — for every environment reason, and
// returns the privileged tools the harness drives.
func requireNATMatrixHost(t *testing.T) (ipBin, nftBin string) {
	t.Helper()
	if os.Getenv(natMatrixEnvVar) != "1" {
		t.Skipf("NAT behaviour matrix is opt-in: run `make nat-matrix` as root (%s=1)", natMatrixEnvVar)
	}
	if runtime.GOOS != "linux" {
		t.Skipf("NAT behaviour matrix needs Linux network namespaces, not %s", runtime.GOOS)
	}
	if os.Geteuid() != 0 {
		t.Skip("NAT behaviour matrix needs root: network namespaces, veth pairs and nftables rules are privileged (`sudo -E make nat-matrix`)")
	}
	ipBin = natTool(t, "ip")
	nftBin = natTool(t, "nft")

	// Root is not sufficient everywhere: a container without CAP_NET_ADMIN, or
	// with /run/netns unwritable, fails here rather than halfway through the
	// wiring.
	probe := "hnm-probe-" + strconv.Itoa(os.Getpid())
	if out, err := exec.Command(ipBin, "netns", "add", probe).CombinedOutput(); err != nil {
		t.Skipf("network namespace creation is denied here (%v): %s", err, strings.TrimSpace(string(out)))
	}
	_ = exec.Command(ipBin, "netns", "del", probe).Run()
	return ipBin, nftBin
}

// natTool resolves one privileged networking tool. A root shell often has no
// /usr/sbin on PATH, so the well-known locations are tried before the suite
// gives up on the environment.
func natTool(t *testing.T, name string) string {
	t.Helper()
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/local/sbin"} {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	t.Skipf("%s(8) is not installed; the NAT behaviour matrix cannot build its topology without it", name)
	return ""
}

// natLab owns the namespaces, links and rulesets of one cell.
type natLab struct {
	t   *testing.T
	ip  string
	nft string

	inet     string
	relayNAT string
	relay    string
	phoneNAT string
	phone    string

	inetToRelay string
	relayToInet string
	inetToPhone string
	phoneToInet string
	relayLAN    string
	relayHost   string
	phoneLAN    string
	phoneHost   string
}

// newNATLab builds the topology. Names carry a random suffix so two harnesses on
// one machine cannot collide, and interface names stay inside the 15-character
// kernel limit.
func newNATLab(t *testing.T, ipBin, nftBin string) *natLab {
	t.Helper()
	suffix := natSuffix(t)
	l := &natLab{
		t:   t,
		ip:  ipBin,
		nft: nftBin,

		inet:     "hnm-" + suffix + "-inet",
		relayNAT: "hnm-" + suffix + "-rnat",
		relay:    "hnm-" + suffix + "-relay",
		phoneNAT: "hnm-" + suffix + "-pnat",
		phone:    "hnm-" + suffix + "-phone",

		inetToRelay: suffix + "ir",
		relayToInet: suffix + "ri",
		inetToPhone: suffix + "ip",
		phoneToInet: suffix + "pi",
		relayLAN:    suffix + "rl",
		relayHost:   suffix + "lr",
		phoneLAN:    suffix + "pf",
		phoneHost:   suffix + "fp",
	}
	// Registered before anything exists: a failure halfway through the wiring
	// must still tear down what was already built.
	t.Cleanup(l.teardown)

	for _, ns := range l.namespaces() {
		l.runIP("netns", "add", ns)
		l.runIP("-n", ns, "link", "set", "lo", "up")
	}

	l.link(l.inet, l.inetToRelay, l.relayNAT, l.relayToInet)
	l.link(l.inet, l.inetToPhone, l.phoneNAT, l.phoneToInet)
	l.link(l.relayNAT, l.relayLAN, l.relay, l.relayHost)
	l.link(l.phoneNAT, l.phoneLAN, l.phone, l.phoneHost)

	l.addr(l.inet, l.inetToRelay, natInetRelaySide+"/30")
	l.addr(l.inet, l.inetToPhone, natInetPhoneSide+"/30")
	l.addr(l.inet, "lo", natGatewayAddr+"/32")

	l.addr(l.relayNAT, l.relayToInet, natRelayExtAddr+"/30")
	l.addr(l.relayNAT, l.relayLAN, natRelayGatewayAddr+"/24")
	l.route(l.relayNAT, natInetRelaySide)

	l.addr(l.phoneNAT, l.phoneToInet, natPhoneExtAddr+"/30")
	l.addr(l.phoneNAT, l.phoneLAN, natPhoneGatewayAddr+"/24")
	l.route(l.phoneNAT, natInetPhoneSide)

	l.addr(l.relay, l.relayHost, natRelayHostAddr+"/24")
	l.route(l.relay, natRelayGatewayAddr)

	l.addr(l.phone, l.phoneHost, natPhoneHostAddr+"/24")
	l.route(l.phone, natPhoneGatewayAddr)

	for _, ns := range []string{l.inet, l.relayNAT, l.phoneNAT} {
		l.forward(ns)
	}
	return l
}

func (l *natLab) namespaces() []string {
	return []string{l.inet, l.relayNAT, l.relay, l.phoneNAT, l.phone}
}

// applyNAT installs the cell's behaviour on both home routers. mgmtPort is
// forwarded through the relay's NAT so the phone namespace can read the relay's
// own /healthz; it is a TCP-only pinhole and therefore invisible to the UDP
// hole punching under test.
func (l *natLab) applyNAT(cell natCell, mgmtPort int) {
	l.t.Helper()
	l.applyRuleset(l.relayNAT, l.natRuleset(cell, l.relayLAN, l.relayToInet, natRelayHostAddr, natPhoneExtAddr, mgmtPort))
	l.applyRuleset(l.phoneNAT, l.natRuleset(cell, l.phoneLAN, l.phoneToInet, natPhoneHostAddr, natRelayExtAddr, 0))
}

// applyRuleset loads one router's whole ruleset atomically, so a rejected line
// can never leave a half-configured NAT that the cell would then measure.
func (l *natLab) applyRuleset(ns, ruleset string) {
	l.t.Helper()
	cmd := l.inNS(ns, l.nft, "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	if out, err := cmd.CombinedOutput(); err != nil {
		l.t.Fatalf("nft ruleset in %s: %v\n%s\nruleset was:\n%s", ns, err, out, ruleset)
	}
}

// natRuleset renders one home router.
//
// Mapping: plain masquerade keeps the source port across destinations, which is
// endpoint-independent ("cone") mapping. `masquerade random` derives the source
// port from a hash that includes the destination, so every destination gets a
// different external port — a symmetric NAT, stable per destination the way real
// hardware is.
//
// Filtering: conntrack alone only admits the exact remote address and port that
// was written to, which is address-and-port-dependent filtering. The
// address-dependent variant adds a pinhole set: every destination this LAN sends
// UDP to becomes an address whose datagrams are admitted from any port. The
// pinhole is expressed as a port-preserving DNAT, so it only delivers when the
// mapping preserved the port too — see the comment on the filtering assertion in
// natmatrix_test.go.
//
// The router's own WAN input is closed, which is not decoration: an unsolicited
// datagram addressed to the external address would otherwise be delivered
// locally, and the conntrack entry that local delivery confirms then owns the
// very tuple the router needs to keep its own source port — so the next outbound
// datagram from that port gets rewritten and both peers end up aiming at ports
// nobody listens on. Real home routers drop WAN input, and dropping it before
// conntrack confirmation is what keeps the mapping behaviour honest here.
func (l *natLab) natRuleset(cell natCell, lanIf, extIf, hostAddr, peerExt string, mgmtPort int) string {
	var mapping string
	switch cell.mapping {
	case natMappingIndependent:
		mapping = "masquerade"
	case natMappingSymmetric:
		mapping = "masquerade random"
	default:
		l.t.Fatalf("unknown mapping behaviour %q", cell.mapping)
	}
	pinhole := false
	switch cell.filtering {
	case natFilterAddress:
		pinhole = true
	case natFilterAddressPort:
	default:
		l.t.Fatalf("unknown filtering behaviour %q", cell.filtering)
	}

	var b strings.Builder
	b.WriteString("table ip herdrnat {\n")
	if pinhole {
		b.WriteString("\tset punched {\n\t\ttype ipv4_addr\n\t\tflags dynamic, timeout\n\t\ttimeout 10m\n\t}\n")
	}

	b.WriteString("\tchain prerouting {\n\t\ttype nat hook prerouting priority dstnat; policy accept;\n")
	if mgmtPort > 0 {
		fmt.Fprintf(&b, "\t\tiifname %q tcp dport %d dnat to %s\n", extIf, mgmtPort, hostAddr)
	}
	if pinhole {
		fmt.Fprintf(&b, "\t\tiifname %q ip protocol udp ip saddr @punched counter dnat to %s\n", extIf, hostAddr)
	}
	b.WriteString("\t}\n")

	b.WriteString("\tchain postrouting {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
	fmt.Fprintf(&b, "\t\toifname %q %s\n\t}\n", extIf, mapping)

	// A verdictless counter ahead of conntrack says how many peer datagrams
	// physically arrived, which the forward chain alone cannot distinguish from
	// "never sent".
	fmt.Fprintf(&b, "\tchain arrivals {\n\t\ttype filter hook prerouting priority -300; policy accept;\n\t\tiifname %q ip saddr %s ip protocol udp counter comment \"punch-arrived\"\n\t}\n", extIf, peerExt)

	// The router's own input: open to its LAN, closed to the internet.
	fmt.Fprintf(&b, "\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t\tiifname %q counter accept\n\t\tct state established,related counter accept\n\t\tcounter drop comment \"router-input-dropped\"\n\t}\n", lanIf)

	// Every forward rule counts, and the drop is spelled out rather than left to
	// the policy: `nft -a list ruleset` in a failure report then says exactly how
	// many datagrams each behaviour admitted and how many it swallowed.
	b.WriteString("\tchain forward {\n\t\ttype filter hook forward priority filter; policy drop;\n")
	// Two verdictless counters bracket the hole punching: how many datagrams this
	// home sent towards the peer's NAT, and how many arrived from it. Together
	// with the trailing drop counter they say whether a failed cell failed
	// because nothing was sent, because nothing arrived, or because what arrived
	// was refused.
	fmt.Fprintf(&b, "\t\tiifname %q ip daddr %s ip protocol udp counter comment \"punch-out\"\n", lanIf, peerExt)
	fmt.Fprintf(&b, "\t\tiifname %q ip saddr %s ip protocol udp counter comment \"punch-in\"\n", extIf, peerExt)
	if pinhole {
		fmt.Fprintf(&b, "\t\tiifname %q ip protocol udp update @punched { ip daddr }\n", lanIf)
	}
	fmt.Fprintf(&b, "\t\tiifname %q counter accept\n", lanIf)
	b.WriteString("\t\tct state established,related counter accept\n")
	if mgmtPort > 0 {
		fmt.Fprintf(&b, "\t\tiifname %q ip daddr %s tcp dport %d counter accept\n", extIf, hostAddr, mgmtPort)
	}
	if pinhole {
		fmt.Fprintf(&b, "\t\tiifname %q ip protocol udp ip saddr @punched ip daddr %s counter accept\n", extIf, hostAddr)
	}
	b.WriteString("\t\tcounter drop\n")
	b.WriteString("\t}\n}\n")
	return b.String()
}

// dumpConntrack renders a router's live flow table, which is the evidence that
// answers "did the checks reach this NAT, and with which external port". Best
// effort like dumpRuleset: kernels without the conntrack procfs simply say so.
func (l *natLab) dumpConntrack(ns string) string {
	out, err := l.inNS(ns, "sh", "-c", "cat /proc/net/nf_conntrack 2>/dev/null | grep udp").CombinedOutput()
	if err != nil && len(out) == 0 {
		return fmt.Sprintf("(no udp flows readable in %s: %v)", ns, err)
	}
	return string(out)
}

// dumpRuleset renders a router's rules and counters for a failure report. It is
// best effort: a diagnostic must never turn one failure into two.
func (l *natLab) dumpRuleset(ns string) string {
	out, err := l.inNS(ns, l.nft, "-a", "list", "ruleset").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("(nft list ruleset in %s failed: %v)", ns, err)
	}
	return string(out)
}

// inNS builds a command that runs inside one namespace. `ip netns exec` execs
// the target in place, so the returned Cmd's process is the target itself and a
// signal reaches it directly.
func (l *natLab) inNS(ns string, args ...string) *exec.Cmd {
	return exec.Command(l.ip, append([]string{"netns", "exec", ns}, args...)...)
}

func (l *natLab) runIP(args ...string) {
	l.t.Helper()
	if out, err := exec.Command(l.ip, args...).CombinedOutput(); err != nil {
		l.t.Fatalf("ip %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func (l *natLab) runInNS(ns string, args ...string) {
	l.t.Helper()
	if out, err := l.inNS(ns, args...).CombinedOutput(); err != nil {
		l.t.Fatalf("in %s: %s: %v\n%s", ns, strings.Join(args, " "), err, out)
	}
}

// link creates a veth pair with both ends born inside their namespaces, so no
// interface ever appears in the caller's namespace, not even for an instant.
func (l *natLab) link(nsA, ifA, nsB, ifB string) {
	l.t.Helper()
	l.runIP("link", "add", ifA, "netns", nsA, "type", "veth", "peer", "name", ifB, "netns", nsB)
}

func (l *natLab) addr(ns, iface, cidr string) {
	l.t.Helper()
	l.runIP("-n", ns, "addr", "add", cidr, "dev", iface)
	l.runIP("-n", ns, "link", "set", iface, "up")
}

func (l *natLab) route(ns, via string) {
	l.t.Helper()
	l.runIP("-n", ns, "route", "add", "default", "via", via)
}

// forward turns one namespace into a router. sysctl(8) is not guaranteed to be
// installed, and /proc/sys/net is scoped to the writing process' network
// namespace, so a plain write does the same job with one fewer dependency.
func (l *natLab) forward(ns string) {
	l.t.Helper()
	l.runInNS(ns, "sh", "-c", "printf 1 > /proc/sys/net/ipv4/ip_forward")
}

// teardown deletes the namespaces, which takes their interfaces, addresses,
// routes, rulesets and conntrack state with them: cleanup cannot half-succeed.
func (l *natLab) teardown() {
	for _, ns := range l.namespaces() {
		_ = exec.Command(l.ip, "netns", "del", ns).Run()
	}
}

func natSuffix(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 2)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(raw)
}
