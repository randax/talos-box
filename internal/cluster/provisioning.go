package cluster

import "fmt"

// CNI identifies the curated cluster network implementation requested during
// provisioning. An empty value preserves the substrate-only path.
type CNI string

const (
	CNICilium  CNI = "cilium"
	CNIFlannel CNI = "flannel"
)

// ProvisioningIntent is the durable, user-requested networking configuration.
// It deliberately contains no progress markers: reconciliation derives those
// from the cluster itself.
type ProvisioningIntent struct {
	CNI    CNI  `json:"cni,omitempty"`
	LB     bool `json:"lb,omitempty"`
	BGP    bool `json:"bgp,omitempty"`
	Hubble bool `json:"hubble,omitempty"`
}

// ProvisioningIntentInput is the wire representation of the provisioning
// knobs. Pointers distinguish omitted values from explicit false values so
// validation can reject any knob supplied without cni.
type ProvisioningIntentInput struct {
	CNI    string `json:"cni,omitempty" yaml:"cni"`
	LB     *bool  `json:"lb,omitempty" yaml:"lb"`
	BGP    *bool  `json:"bgp,omitempty" yaml:"bgp"`
	Hubble *bool  `json:"hubble,omitempty" yaml:"hubble"`
}

// Intent returns the defaulted, validated durable intent.
func (input ProvisioningIntentInput) Intent() (ProvisioningIntent, error) {
	return ParseProvisioningIntent(input.CNI, input.LB, input.BGP, input.Hubble)
}

// Input returns the protocol form of a validated intent. Substrate-only
// intent keeps all knobs absent for compatibility with existing clients.
func (intent ProvisioningIntent) Input() ProvisioningIntentInput {
	if intent.CNI == "" {
		return ProvisioningIntentInput{}
	}
	return ProvisioningIntentInput{
		CNI: string(intent.CNI), LB: boolPointer(intent.LB),
		BGP: boolPointer(intent.BGP), Hubble: boolPointer(intent.Hubble),
	}
}

// ParseProvisioningIntent validates the user-facing CNI knobs and applies
// their defaults. Pointer values preserve whether a knob was supplied, which
// lets callers reject even an explicit false value without cni.
func ParseProvisioningIntent(cni string, lb, bgp, hubble *bool) (ProvisioningIntent, error) {
	if cni == "" {
		switch {
		case lb != nil:
			return ProvisioningIntent{}, fmt.Errorf("lb requires cni: cilium or flannel")
		case bgp != nil:
			return ProvisioningIntent{}, fmt.Errorf("bgp requires cni: cilium")
		case hubble != nil:
			return ProvisioningIntent{}, fmt.Errorf("hubble requires cni: cilium")
		default:
			return ProvisioningIntent{}, nil
		}
	}

	intent := ProvisioningIntent{CNI: CNI(cni), LB: true}
	switch intent.CNI {
	case CNICilium, CNIFlannel:
	default:
		return ProvisioningIntent{}, fmt.Errorf("cni must be one of cilium or flannel, got %q", cni)
	}
	if lb != nil {
		intent.LB = *lb
	}
	if bgp != nil {
		intent.BGP = *bgp
	}
	if hubble != nil {
		intent.Hubble = *hubble
	}
	if intent.BGP && !intent.LB {
		return ProvisioningIntent{}, fmt.Errorf("bgp requires lb: true")
	}
	if intent.BGP && intent.CNI != CNICilium {
		return ProvisioningIntent{}, fmt.Errorf("bgp requires cni: cilium")
	}
	if intent.Hubble && intent.CNI != CNICilium {
		return ProvisioningIntent{}, fmt.Errorf("hubble requires cni: cilium")
	}
	return intent, nil
}

func boolPointer(value bool) *bool { return &value }
