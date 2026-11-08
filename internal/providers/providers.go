// Package providers holds the profiles that map an exposure to a Prometheus query.
package providers

import (
	"bytes"
	_ "embed"
	"fmt"
	"path"
	"slices"
	"strings"
	"text/template"
	"time"

	"sigs.k8s.io/yaml"
)

//go:embed profiles.yaml
var builtin []byte

const (
	KindIngress   = "Ingress"
	KindHTTPRoute = "HTTPRoute"
	KindService   = "Service"
)

type Match struct {
	Kinds              []string `json:"kinds"`
	IngressClasses     []string `json:"ingressClasses"`
	GatewayControllers []string `json:"gatewayControllers"`
	GatewayClasses     []string `json:"gatewayClasses"`
	Annotations        []string `json:"annotations"`
	HostnameSuffixes   []string `json:"hostnameSuffixes"`
}

type Profile struct {
	Name      string `json:"name"`
	Supported bool   `json:"supported"`
	Unit      string `json:"unit"`
	Match     Match  `json:"match"`
	Query     string `json:"query"`
	Health    string `json:"health"`

	tmpl *template.Template
}

// Exposure is the template data for Profile.Query.
type Exposure struct {
	Namespace, Kind, Workload string
	ExposureKind, Service     string
	ServicePorts              []int32
	Ingress, Route            string
	Gateway, GatewayNamespace string
	Hosts                     []string
	LBHostname, LBIP          string
	Annotations               map[string]string
	Window                    string
}

// Facts is what Match looks at; discovery fills it next to the Exposure.
type Facts struct {
	Kind              string // Ingress | HTTPRoute | Service
	IngressClass      string
	GatewayController string
	GatewayClass      string
	Annotations       map[string]string
	LBHostnames       []string
}

// Set is an ordered list of profiles; the first supported match wins.
type Set struct{ profiles []*Profile }

// Parse reads and validates a profile list.
func Parse(b []byte) ([]Profile, error) {
	var ps []Profile
	if err := yaml.UnmarshalStrict(b, &ps); err != nil {
		return nil, fmt.Errorf("parse profiles: %w", err)
	}
	seen := map[string]bool{}
	for i := range ps {
		if err := ps[i].compile(); err != nil {
			return nil, fmt.Errorf("profile %q: %w", ps[i].Name, err)
		}
		if seen[ps[i].Name] {
			return nil, fmt.Errorf("duplicate profile %q", ps[i].Name)
		}
		seen[ps[i].Name] = true
	}
	return ps, nil
}

func (p *Profile) compile() error {
	if p.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(p.Match.Kinds) == 0 {
		return fmt.Errorf("match.kinds is required")
	}
	for _, k := range p.Match.Kinds {
		if k != KindIngress && k != KindHTTPRoute && k != KindService {
			return fmt.Errorf("unknown kind %q", k)
		}
	}
	if !p.Supported {
		return nil
	}
	m := p.Match
	if len(m.IngressClasses)+len(m.GatewayControllers)+len(m.GatewayClasses)+len(m.Annotations)+len(m.HostnameSuffixes) == 0 {
		return fmt.Errorf("a supported profile needs a matcher besides kinds")
	}
	if strings.TrimSpace(p.Health) == "" || strings.TrimSpace(p.Query) == "" {
		return fmt.Errorf("query and health are required")
	}
	t, err := template.New(p.Name).Funcs(template.FuncMap{"lbName": LBName}).Option("missingkey=error").Parse(p.Query)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	p.tmpl = t
	return nil
}

// Load returns user profiles first, then the built-ins; a user profile replaces a built-in of the same name.
func Load(user []Profile) (*Set, error) {
	base, err := Parse(builtin)
	if err != nil {
		return nil, err
	}
	s := &Set{}
	names := map[string]bool{}
	for i := range user {
		if err := user[i].compile(); err != nil {
			return nil, fmt.Errorf("provider %q: %w", user[i].Name, err)
		}
		names[user[i].Name] = true
		s.profiles = append(s.profiles, &user[i])
	}
	for i := range base {
		if !names[base[i].Name] {
			s.profiles = append(s.profiles, &base[i])
		}
	}
	return s, nil
}

// Match returns the first supported profile that fits, or nil.
func (s *Set) Match(f Facts) *Profile {
	for _, p := range s.profiles {
		if p.Supported && p.matches(f) {
			return p
		}
	}
	return nil
}

func (p *Profile) matches(f Facts) bool {
	m := p.Match
	if !slices.Contains(m.Kinds, f.Kind) {
		return false
	}
	if slices.Contains(m.IngressClasses, f.IngressClass) && f.IngressClass != "" {
		return true
	}
	if f.GatewayController != "" && slices.Contains(m.GatewayControllers, f.GatewayController) {
		return true
	}
	if f.GatewayClass != "" {
		for _, g := range m.GatewayClasses {
			if ok, _ := path.Match(g, f.GatewayClass); ok {
				return true
			}
		}
	}
	for _, a := range m.Annotations {
		if _, ok := f.Annotations[a]; ok {
			return true
		}
	}
	for _, h := range f.LBHostnames {
		for _, suf := range m.HostnameSuffixes {
			if strings.HasSuffix(h, suf) {
				return true
			}
		}
	}
	return false
}

// WindowString renders whole seconds, e.g. 604800s; Go's 168h0m0s is not valid PromQL.
func WindowString(d time.Duration) string { return fmt.Sprintf("%ds", int64(d/time.Second)) }

// Render fills the query template for one exposure.
func (p *Profile) Render(e Exposure, window time.Duration) (string, error) {
	e.Window = WindowString(window)
	var b bytes.Buffer
	if err := p.tmpl.Execute(&b, e); err != nil {
		return "", fmt.Errorf("render %s: %w", p.Name, err)
	}
	return b.String(), nil
}

// LBName turns an AWS ELB hostname into the LB name: first DNS label minus the trailing "-<digits>".
func LBName(hostname string) string {
	label, _, _ := strings.Cut(hostname, ".")
	if i := strings.LastIndex(label, "-"); i > 0 && label[i+1:] != "" && strings.Trim(label[i+1:], "0123456789") == "" {
		return label[:i]
	}
	return label
}
