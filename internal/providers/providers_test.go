package providers

import (
	"strings"
	"testing"
	"time"
)

const fixture = `
- name: traefik
  supported: true
  match: {kinds: [Ingress], ingressClasses: [traefik]}
  query: 'sum(increase(t{service=~"{{.Namespace}}-{{.Service}}-.*"}[{{.Window}}]))'
  health: 'up{job="traefik"}'
- name: gke
  supported: true
  match: {kinds: [HTTPRoute, Ingress], gatewayClasses: ["gke-l7-*"], annotations: ["ingress.kubernetes.io/url-map"]}
  query: 'gke'
  health: 'up'
- name: aws
  supported: true
  match: {kinds: [Service], hostnameSuffixes: [".elb.amazonaws.com"]}
  query: 'sum(r{lb=~"app/{{lbName .LBHostname}}/.*"})'
  health: 'up'
- name: envoy
  supported: true
  match: {kinds: [HTTPRoute], gatewayControllers: ["gateway.envoyproxy.io/gatewayclass-controller"]}
  query: 'e'
  health: 'up'
- name: nodocs
  supported: false
  match: {kinds: [Ingress], ingressClasses: [nope]}
`

func TestMatch(t *testing.T) {
	ps, err := Parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	s := &Set{}
	for i := range ps {
		s.profiles = append(s.profiles, &ps[i])
	}
	tests := []struct {
		name string
		f    Facts
		want string
	}{
		{"ingress class", Facts{Kind: "Ingress", IngressClass: "traefik"}, "traefik"},
		{"kind mismatch", Facts{Kind: "Service", IngressClass: "traefik"}, ""},
		{"unsupported never matches", Facts{Kind: "Ingress", IngressClass: "nope"}, ""},
		{"empty class no match", Facts{Kind: "Ingress"}, ""},
		{"gateway class glob", Facts{Kind: "HTTPRoute", GatewayClass: "gke-l7-global-external-managed"}, "gke"},
		{"annotation present", Facts{Kind: "Ingress", Annotations: map[string]string{"ingress.kubernetes.io/url-map": ""}}, "gke"},
		{"controller exact", Facts{Kind: "HTTPRoute", GatewayController: "gateway.envoyproxy.io/gatewayclass-controller"}, "envoy"},
		{"hostname suffix", Facts{Kind: "Service", LBHostnames: []string{"x-1.eu-west-1.elb.amazonaws.com"}}, "aws"},
		{"nothing", Facts{Kind: "Service", LBHostnames: []string{"1.2.3.4"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ""
			if p := s.Match(tt.f); p != nil {
				got = p.Name
			}
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestLoadOrderAndOverride(t *testing.T) {
	user := []Profile{
		{Name: "mine", Supported: true, Match: Match{Kinds: []string{"Ingress"}, IngressClasses: []string{"traefik"}}, Query: "mine", Health: "up"},
		{Name: "traefik", Supported: true, Match: Match{Kinds: []string{"Ingress"}, IngressClasses: []string{"other"}}, Query: "override", Health: "up"},
	}
	s, err := Load(user)
	if err != nil {
		t.Fatal(err)
	}
	if p := s.Match(Facts{Kind: "Ingress", IngressClass: "traefik"}); p == nil || p.Name != "mine" {
		t.Fatalf("user profile should win, got %+v", p)
	}
	if p := s.Match(Facts{Kind: "Ingress", IngressClass: "other"}); p == nil || p.Query != "override" {
		t.Fatalf("same-name user profile should replace built-in, got %+v", p)
	}
	n := 0
	for _, p := range s.profiles {
		if p.Name == "traefik" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("traefik appears %d times", n)
	}
}

func TestRender(t *testing.T) {
	ps, _ := Parse([]byte(fixture))
	got, err := ps[0].Render(Exposure{Namespace: "ns", Service: "web"}, 168*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if want := `sum(increase(t{service=~"ns-web-.*"}[604800s]))`; got != want {
		t.Fatalf("got %s", got)
	}
	got, err = ps[2].Render(Exposure{LBHostname: "k8s-default-web-0123456789abcdef-1234567890.us-east-1.elb.amazonaws.com"}, time.Hour)
	if err != nil || got != `sum(r{lb=~"app/k8s-default-web-0123456789abcdef/.*"})` {
		t.Fatalf("got %s, %v", got, err)
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string]string{
		"bad template":  "- {name: a, supported: true, match: {kinds: [Ingress], ingressClasses: [x]}, query: '{{.Nope', health: up}",
		"no health":     "- {name: a, supported: true, match: {kinds: [Ingress], ingressClasses: [x]}, query: q}",
		"bad kind":      "- {name: a, match: {kinds: [Pod]}}",
		"no matcher":    "- {name: a, supported: true, match: {kinds: [Ingress]}, query: q, health: up}",
		"duplicate":     "- {name: a, match: {kinds: [Ingress]}}\n- {name: a, match: {kinds: [Ingress]}}",
		"unknown field": "- {name: a, match: {kinds: [Ingress]}, bogus: 1}",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(in)); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestRenderUnknownField(t *testing.T) {
	ps, err := Parse([]byte("- {name: a, supported: true, match: {kinds: [Ingress], ingressClasses: [x]}, query: '{{.Nope}}', health: up}"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ps[0].Render(Exposure{}, time.Hour); err == nil || !strings.Contains(err.Error(), "Nope") {
		t.Fatalf("got %v", err)
	}
}

func TestLBName(t *testing.T) {
	tests := map[string]string{
		"k8s-default-web-0123456789abcdef-1234567890.us-east-1.elb.amazonaws.com": "k8s-default-web-0123456789abcdef",
		"a1b2c3d4-987654321.eu-west-1.elb.amazonaws.com":                          "a1b2c3d4",
		"internal-foo-123.us-east-1.elb.amazonaws.com":                            "internal-foo",
		"nodigits.elb.amazonaws.com":                                              "nodigits",
		"trailing-abc.elb.amazonaws.com":                                          "trailing-abc",
		"":                                                                        "",
	}
	for in, want := range tests {
		if got := LBName(in); got != want {
			t.Errorf("LBName(%q)=%q want %q", in, got, want)
		}
	}
}

// Guards whatever profiles.yaml ships: every profile must parse and compile.
func TestEmbeddedProfiles(t *testing.T) {
	ps, err := Parse(builtin)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) == 0 {
		t.Fatal("no profiles")
	}
	for _, p := range ps {
		if p.Supported && (p.Health == "" || p.tmpl == nil) {
			t.Errorf("%s: incomplete", p.Name)
		}
	}
	if _, err := Load(nil); err != nil {
		t.Fatal(err)
	}
}
