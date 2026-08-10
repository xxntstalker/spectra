package openapi

// Owner is what a specification says about the people behind it.
//
// OpenAPI already has a place for this — info.contact and externalDocs — so a
// team that fills it in gets picked up with no integration at all. The x-
// extensions cover what the standard fields do not: which team, which repo,
// which people.
type Owner struct {
	Team    string
	Contact string
	Repo    string
	Owners  []string
}

// Owner extracts contacts from the document. Everything here is optional and
// usually absent; an empty result is the normal case, not a failure.
func (d *Doc) Owner() Owner {
	var o Owner
	info, _ := d.Root["info"].(map[string]any)

	if contact, ok := info["contact"].(map[string]any); ok {
		o.Team, _ = contact["name"].(string)
		if email, _ := contact["email"].(string); email != "" {
			o.Contact = email
		}
		// contact.url points at the team, externalDocs at the code. When only
		// the former exists it is still better than nothing.
		if url, _ := contact["url"].(string); url != "" {
			o.Repo = url
		}
	}

	// Vendor extensions win over the generic contact block: they say exactly
	// what they mean.
	if team, _ := info["x-team"].(string); team != "" {
		o.Team = team
	}
	if repo, _ := info["x-repo"].(string); repo != "" {
		o.Repo = repo
	}
	if chat, _ := info["x-chat"].(string); chat != "" && o.Contact == "" {
		o.Contact = chat
	}
	for _, v := range toSlice(info["x-owners"]) {
		if name, ok := v.(string); ok && name != "" {
			o.Owners = append(o.Owners, name)
		}
	}

	// externalDocs is where the source repository belongs, so it outranks
	// contact.url.
	if docs, ok := d.Root["externalDocs"].(map[string]any); ok {
		if url, _ := docs["url"].(string); url != "" {
			o.Repo = url
		}
	}
	return o
}
