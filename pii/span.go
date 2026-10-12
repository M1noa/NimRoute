package pii

import "sort"

// label set: rampart taxonomy + extras for keys/secrets.
type Label string

const (
	LSSN              Label = "SSN"
	LCreditCard       Label = "CREDIT_CARD"
	LIPAddress        Label = "IP_ADDRESS"
	LGivenName        Label = "GIVEN_NAME"
	LSurname          Label = "SURNAME"
	LEmail            Label = "EMAIL"
	LPhone            Label = "PHONE"
	LURL              Label = "URL"
	LTaxID            Label = "TAX_ID"
	LBankAccount      Label = "BANK_ACCOUNT"
	LRoutingNumber    Label = "ROUTING_NUMBER"
	LGovernmentID     Label = "GOVERNMENT_ID"
	LPassport         Label = "PASSPORT"
	LDriversLicense   Label = "DRIVERS_LICENSE"
	LBuildingNumber   Label = "BUILDING_NUMBER"
	LStreetName       Label = "STREET_NAME"
	LSecondaryAddress Label = "SECONDARY_ADDRESS"
	LCity             Label = "CITY"
	LState            Label = "STATE"
	LZipCode          Label = "ZIP_CODE"
	LAPIKey           Label = "API_KEY"
	LPrivateKey       Label = "PRIVATE_KEY"
	LJWT              Label = "JWT"
	LIBAN             Label = "IBAN"
	LCustom           Label = "CUSTOM"
)

type Span struct {
	Start  int
	End    int
	Label  Label
	Score  float64
	Source string // heuristic | ner | custom | secret
	Text   string
}

var keepDefault = map[Label]bool{
	LCity: true, LState: true, LZipCode: true,
}

func mergeSpans(spans []Span) []Span {
	if len(spans) == 0 {
		return nil
	}
	sorted := append([]Span(nil), spans...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Start != sorted[j].Start {
			return sorted[i].Start < sorted[j].Start
		}
		return sorted[i].End > sorted[j].End
	})
	var out []Span
	for _, s := range sorted {
		if len(out) == 0 {
			out = append(out, s)
			continue
		}
		p := &out[len(out)-1]
		if s.Start >= p.End {
			out = append(out, s)
			continue
		}
		// overlap: the winner keeps its exact bounds (widening to the
		// union destroys one value on restore), and the loser's
		// non-overlapping tail survives as its own span so nothing
		// unmasks by being dropped.
		winner, loser := *p, s
		if prefer(s, *p) {
			winner, loser = s, *p
		}
		*p = winner
		if loser.Start < winner.Start {
			out = append(out, Span{loser.Start, winner.Start, loser.Label, loser.Score, loser.Source, ""})
		} else if loser.End > winner.End {
			out = append(out, Span{winner.End, loser.End, loser.Label, loser.Score, loser.Source, ""})
		}
	}
	// clipped tails append out of order: restore start ordering so
	// right-to-left masking stays valid.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return out[i].End > out[j].End
	})
	return out
}

func prefer(a, b Span) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	// entities win over flat terms on overlap (documented precedence).
	ae, be := a.Source == "custom-entity", b.Source == "custom-entity"
	if ae != be {
		return ae
	}
	la, lb := a.End-a.Start, b.End-b.Start
	if la != lb {
		return la > lb
	}
	return a.Source == "heuristic"
}

func applyPolicy(spans []Span, keep map[Label]bool) []Span {
	if keep == nil {
		keep = keepDefault
	}
	m := mergeSpans(spans)
	var out []Span
	for _, s := range m {
		if !keep[s.Label] {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start > out[j].Start })
	return out
}
