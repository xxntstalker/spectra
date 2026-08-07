package openapi

import (
	"bytes"
	"encoding/json"
	"sort"
)

// Change kinds.
const (
	KindAdded    = "added"
	KindRemoved  = "removed"
	KindModified = "modified"
)

// Detail is one concrete difference inside an operation. It carries a machine
// code rather than prose so the UI owns the wording (and its language) while
// the backend stays presentation-neutral.
type Detail struct {
	Code     string `json:"code"`
	Subject  string `json:"subject,omitempty"`
	Breaking bool   `json:"breaking,omitempty"`
}

// OpChange is what happened to a single endpoint between two spec revisions.
type OpChange struct {
	Kind     string   `json:"kind"`
	Key      string   `json:"key"`
	Method   string   `json:"method"`
	Path     string   `json:"path"`
	Summary  string   `json:"summary,omitempty"`
	Breaking bool     `json:"breaking"`
	Details  []Detail `json:"details,omitempty"`
}

// Diff is the comparison of two spec revisions.
type Diff struct {
	Changes  []OpChange `json:"changes"`
	Notes    []Detail   `json:"notes,omitempty"`
	Breaking bool       `json:"breaking"`
	Added    int        `json:"added"`
	Removed  int        `json:"removed"`
	Modified int        `json:"modified"`
}

// Empty reports whether the two revisions are indistinguishable to a caller.
func (d *Diff) Empty() bool { return len(d.Changes) == 0 && len(d.Notes) == 0 }

// Compare diffs two documents.
//
// "Breaking" here is a heuristic on the subset of changes that reliably break
// existing callers: an endpoint or a success response disappearing, and new or
// newly-required inputs. It is deliberately conservative — it will not catch
// semantic changes (a field's meaning changing, tightened validation inside a
// schema), and it is meant to draw attention, not to gate a release.
func Compare(oldDoc, newDoc *Doc) *Diff {
	oldIx := oldDoc.BuildIndex()
	newIx := newDoc.BuildIndex()
	d := &Diff{}

	seen := map[string]bool{}
	for _, key := range newIx.Keys() {
		seen[key] = true
		newOp := newIx.Ops[key]
		oldOp, existed := oldIx.Ops[key]
		if !existed {
			d.Changes = append(d.Changes, OpChange{
				Kind: KindAdded, Key: key, Method: newOp.Method,
				Path: newOp.Path, Summary: newOp.Summary,
			})
			continue
		}
		if oldOp.Hash == newOp.Hash {
			continue
		}
		change := OpChange{
			Kind: KindModified, Key: key, Method: newOp.Method,
			Path: newOp.Path, Summary: newOp.Summary,
			Details: compareOps(oldOp, newOp),
		}
		for _, det := range change.Details {
			if det.Breaking {
				change.Breaking = true
			}
		}
		d.Changes = append(d.Changes, change)
	}
	for _, key := range oldIx.Keys() {
		if seen[key] {
			continue
		}
		oldOp := oldIx.Ops[key]
		d.Changes = append(d.Changes, OpChange{
			Kind: KindRemoved, Key: key, Method: oldOp.Method,
			Path: oldOp.Path, Summary: oldOp.Summary, Breaking: true,
			Details: []Detail{{Code: "op.removed", Breaking: true}},
		})
	}

	sort.Slice(d.Changes, func(i, j int) bool {
		if d.Changes[i].Kind != d.Changes[j].Kind {
			return kindRank(d.Changes[i].Kind) < kindRank(d.Changes[j].Kind)
		}
		return d.Changes[i].Key < d.Changes[j].Key
	})

	for _, c := range d.Changes {
		switch c.Kind {
		case KindAdded:
			d.Added++
		case KindRemoved:
			d.Removed++
		default:
			d.Modified++
		}
		if c.Breaking {
			d.Breaking = true
		}
	}

	d.Notes = compareInfo(oldDoc, newDoc)

	// A revision is recorded whenever the hash moves, but the checks above only
	// look at operations and the info block. Root-level sections — security,
	// tags, externalDocs, 2.0's consumes/produces — would otherwise produce a
	// change card with nothing on it.
	if d.Empty() && !identical(oldDoc, newDoc) {
		d.Notes = append(d.Notes, Detail{Code: "spec.other.changed"})
	}
	return d
}

func identical(a, b *Doc) bool {
	left, errLeft := json.Marshal(a.Root)
	right, errRight := json.Marshal(b.Root)
	if errLeft != nil || errRight != nil {
		return false
	}
	return bytes.Equal(left, right)
}

func kindRank(kind string) int {
	switch kind {
	case KindRemoved:
		return 0
	case KindAdded:
		return 1
	default:
		return 2
	}
}

func compareOps(oldOp, newOp *Operation) []Detail {
	var details []Detail

	oldParams := paramMap(oldOp.Params)
	newParams := paramMap(newOp.Params)
	for _, k := range sortedKeys(newParams) {
		np := newParams[k]
		op, existed := oldParams[k]
		switch {
		case !existed && np.Required:
			details = append(details, Detail{Code: "param.added.required", Subject: k, Breaking: true})
		case !existed:
			details = append(details, Detail{Code: "param.added", Subject: k})
		case !op.Required && np.Required:
			details = append(details, Detail{Code: "param.required.on", Subject: k, Breaking: true})
		case op.Required && !np.Required:
			details = append(details, Detail{Code: "param.required.off", Subject: k})
		}
	}
	for _, k := range sortedKeys(oldParams) {
		if _, still := newParams[k]; still {
			continue
		}
		if oldParams[k].Required {
			details = append(details, Detail{Code: "param.removed.required", Subject: k, Breaking: true})
		} else {
			details = append(details, Detail{Code: "param.removed", Subject: k})
		}
	}

	for _, code := range diffSets(oldOp.Responses, newOp.Responses) {
		details = append(details, Detail{Code: "response.added", Subject: code})
	}
	for _, code := range diffSets(newOp.Responses, oldOp.Responses) {
		breaking := len(code) > 0 && code[0] == '2'
		c := "response.removed"
		if breaking {
			c = "response.removed.success"
		}
		details = append(details, Detail{Code: c, Subject: code, Breaking: breaking})
	}

	if !oldOp.BodyRequired && newOp.BodyRequired {
		details = append(details, Detail{Code: "body.required.on", Breaking: true})
	}
	if oldOp.BodyRequired && !newOp.BodyRequired {
		details = append(details, Detail{Code: "body.required.off"})
	}
	for _, f := range diffSets(oldOp.BodyFields, newOp.BodyFields) {
		details = append(details, Detail{Code: "body.field.added.required", Subject: f, Breaking: true})
	}
	for _, f := range diffSets(newOp.BodyFields, oldOp.BodyFields) {
		details = append(details, Detail{Code: "body.field.removed.required", Subject: f})
	}

	if !oldOp.Deprecated && newOp.Deprecated {
		details = append(details, Detail{Code: "op.deprecated.on"})
	}
	if oldOp.Deprecated && !newOp.Deprecated {
		details = append(details, Detail{Code: "op.deprecated.off"})
	}
	if oldOp.Summary != newOp.Summary {
		details = append(details, Detail{Code: "op.summary.changed"})
	}

	if len(details) == 0 {
		// The resolved hashes differ but none of the checks above fired: the
		// change is inside a schema, an example or a description. Worth showing
		// as "changed" rather than swallowing it.
		details = append(details, Detail{Code: "op.schema.changed"})
	}
	return details
}

func compareInfo(oldDoc, newDoc *Doc) []Detail {
	var notes []Detail
	if o, n := oldDoc.APIVersion(), newDoc.APIVersion(); o != n {
		notes = append(notes, Detail{Code: "spec.version.changed", Subject: o + " → " + n})
	}
	if o, n := oldDoc.SpecVersion(), newDoc.SpecVersion(); o != n {
		notes = append(notes, Detail{Code: "spec.openapi.changed", Subject: o + " → " + n})
	}
	if o, n := oldDoc.Title(), newDoc.Title(); o != n {
		notes = append(notes, Detail{Code: "spec.title.changed", Subject: o + " → " + n})
	}
	return notes
}

func paramMap(params []Param) map[string]Param {
	out := make(map[string]Param, len(params))
	for _, p := range params {
		out[p.In+":"+p.Name] = p
	}
	return out
}

// diffSets returns the items present in b but not in a.
func diffSets(a, b []string) []string {
	in := make(map[string]bool, len(a))
	for _, s := range a {
		in[s] = true
	}
	var out []string
	for _, s := range b {
		if !in[s] {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
