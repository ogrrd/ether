package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// upgradeShippedQuality moves an install still on the quality defaults
// releases up to v0.3.1 shipped (1080p at home and away, an Apple TV rule
// for 4K, an 8 Mbps remote cap) to the current ones: no server cap at
// home or away, each device held to what it can play. Anything the owner
// changed is left alone. It reports whether cfg changed.
func upgradeShippedQuality(cfg *Config) bool {
	q := &cfg.Quality
	if q.Default != "1080p" || q.Remote != "1080p" || len(q.Rules) != 1 || q.Rules[0] != (QualityRule{Product: "Apple TV", Profile: "4K"}) {
		return false
	}
	uhd := -1
	for i, p := range q.Profiles {
		if p.Name == "4K" {
			uhd = i
		}
	}
	if uhd < 0 {
		return false
	}
	q.Default, q.Remote, q.Rules = "4K", "", nil
	if q.Profiles[uhd].MinHeight == 1080 {
		q.Profiles[uhd].MinHeight = 720
	}
	if cfg.Transcode.RemoteMaxBitrateKbps == 8000 {
		cfg.Transcode.RemoteMaxBitrateKbps = 0
	}
	return true
}

// The per-media At home and Away fields of each profile media.
var mediaDefaultFields = map[string]struct{ home, away string }{
	core.MediaMovie: {"defaultMovie", "remoteMovie"},
	core.MediaTV:    {"defaultTv", "remoteTv"},
}

// normalizeQuality settles q into the one shape this version accepts; see
// normalizeQualityDocument. It works on q's JSON form so the typed and the
// document paths cannot drift apart, and reports what it changed.
func normalizeQuality(q *Quality) []string {
	b, err := json.Marshal(q)
	if err != nil {
		return nil
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&doc) != nil {
		return nil
	}
	notes := normalizeQualityDocument(doc)
	if len(notes) == 0 {
		return nil
	}
	if b, err = json.Marshal(doc); err != nil {
		return nil
	}
	var out Quality
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	*q = out
	return notes
}

// normalizeSharedDocument normalizes the quality section of a settings
// document in place, keeping fields this version does not know.
func normalizeSharedDocument(doc map[string]any) []string {
	quality, ok := doc["quality"].(map[string]any)
	if !ok {
		return nil
	}
	return normalizeQualityDocument(quality)
}

// normalizeQualityDocument gives every profile of a quality section exactly
// one media, keeps names unique per media and points every default and client
// rule at a profile that serves its media. It runs on every load (config.json
// and the cluster's shared settings), so it must be idempotent: a settled
// section comes back unchanged and with no notes.
//
// Splitting. A profile saved before profiles were per media (v0.3.4 and
// earlier) served movies and TV shows alike; it becomes a movie profile and
// a TV profile with the same name and settings, the TV one right after it.
// Names are unique per media and every reference to a profile (client
// rules, the At home and Away defaults, AI tools) is a name resolved with
// the title's media, so each reference keeps resolving to the same settings
// and no reference is rewritten.
//
// A name repeated among profiles without a media cannot come from v0.3.4,
// which rejected duplicate names: it is a per-media list whose media was
// lost (a release that predates per-media profiles saved over it, such as
// a downgrade or an older cluster node). v0.3.5 writes the movie profile of
// a name before the TV one, so the n-th unmarked copy of a name is the
// profile of the n-th media. Splitting each copy into every media instead
// made two movie and two TV profiles of every name, which validation then
// refused, so no change (deleting one included) could be saved.
//
// Duplicates. A second profile with the media and name of an earlier one is
// dropped when its settings are the same and renamed "<name> (2)" when they
// differ, so an owner's customization is never lost. The first keeps the
// name: it is the one every reference already resolved to.
func normalizeQualityDocument(quality map[string]any) []string {
	profiles, ok := quality["profiles"].([]any)
	if !ok {
		return nil
	}
	var notes []string
	changed := false

	unmarked := map[string]int{}
	for _, item := range profiles {
		if p, ok := item.(map[string]any); ok && docString(p, "mediaType") == "" {
			unmarked[docString(p, "name")]++
		}
	}
	split := make([]any, 0, len(profiles))
	seen := map[string]int{}
	for _, item := range profiles {
		p, ok := item.(map[string]any)
		if !ok || docString(p, "mediaType") != "" {
			split = append(split, item)
			continue
		}
		changed = true
		name := docString(p, "name")
		media := core.ProfileMediaTypes
		if i := seen[name]; unmarked[name] > 1 && i < len(media) {
			if i == 0 {
				notes = append(notes, fmt.Sprintf("profile %q appeared %d times without a media; restored its copies as the %s profiles in order", name, unmarked[name], strings.Join(media, ", ")))
			}
			media = media[i : i+1]
		} else {
			notes = append(notes, fmt.Sprintf("split profile %q into %s profiles", name, strings.Join(media, " and ")))
		}
		seen[name]++
		for _, m := range media {
			c := maps.Clone(p)
			c["mediaType"] = m
			split = append(split, c)
		}
	}

	type key struct{ media, name string }
	taken := map[key]bool{}
	for _, item := range split {
		if p, ok := item.(map[string]any); ok {
			taken[key{docString(p, "mediaType"), docString(p, "name")}] = true
		}
	}
	first := map[key][]map[string]any{}
	out := make([]any, 0, len(split))
	for _, item := range split {
		p, ok := item.(map[string]any)
		if !ok || docString(p, "name") == "" {
			out = append(out, item)
			continue
		}
		media, name := docString(p, "mediaType"), docString(p, "name")
		k := key{media, name}
		earlier := first[k]
		if len(earlier) == 0 {
			first[k] = []map[string]any{p}
			out = append(out, p)
			continue
		}
		changed = true
		if slices.ContainsFunc(earlier, func(e map[string]any) bool { return reflect.DeepEqual(e, p) }) {
			notes = append(notes, fmt.Sprintf("removed a duplicate %s profile %q", media, name))
			continue
		}
		first[k] = append(earlier, p)
		renamed := name
		for n := 2; taken[key{media, renamed}]; n++ {
			renamed = fmt.Sprintf("%s (%d)", name, n)
		}
		taken[key{media, renamed}] = true
		c := maps.Clone(p)
		c["name"] = renamed
		out = append(out, c)
		notes = append(notes, fmt.Sprintf("a second %s profile %q has different settings; renamed it %q", media, name, renamed))
	}
	if changed {
		quality["profiles"] = out
	}

	serves := func(name, media string) bool {
		for _, item := range out {
			if p, ok := item.(map[string]any); ok && docString(p, "name") == name && (media == "" || docString(p, "mediaType") == media) {
				return true
			}
		}
		return false
	}
	firstFor := func(media string) string {
		for _, item := range out {
			if p, ok := item.(map[string]any); ok && (media == "" || docString(p, "mediaType") == media) {
				return docString(p, "name")
			}
		}
		return ""
	}
	repoint := func(field, to string) {
		notes = append(notes, fmt.Sprintf("%s named a missing profile %q; now %q", field, docString(quality, field), to))
		quality[field] = to
	}
	for _, m := range core.ProfileMediaTypes {
		f, ok := mediaDefaultFields[m]
		if !ok {
			continue
		}
		if v := docString(quality, f.home); v != "" && !serves(v, m) {
			repoint(f.home, firstFor(m))
		}
		if v := docString(quality, f.away); v != "" && !serves(v, m) {
			repoint(f.away, "")
		}
	}
	// Each repair resolves as the missing name already did at playback: a
	// missing At home profile is the media's first one, a missing Away one
	// the At home one, and a client rule naming a removed profile falls back
	// to the At home profile while keeping the rule's device matchers and
	// limits. Validation refuses a name that serves nothing, so left alone
	// it would block every quality change.
	if len(out) > 0 {
		if v := docString(quality, "default"); v != "" && !serves(v, "") {
			repoint("default", "")
		}
		if v := docString(quality, "remote"); v != "" && !serves(v, "") {
			repoint("remote", "")
		}
		homeFor := func(media string) string {
			if f, ok := mediaDefaultFields[media]; ok {
				if v := docString(quality, f.home); v != "" && serves(v, media) {
					return v
				}
			}
			if v := docString(quality, "default"); v != "" && serves(v, media) {
				return v
			}
			return firstFor(media)
		}
		if rules, ok := quality["rules"].([]any); ok && len(rules) > 0 {
			repaired := make([]any, 0, len(rules))
			rulesChanged := false
			for i, item := range rules {
				r, ok := item.(map[string]any)
				if !ok {
					repaired = append(repaired, item)
					continue
				}
				media, prof := docString(r, "mediaType"), docString(r, "profile")
				if (media != "" && !core.IsProfileMedia(media)) || prof == "" || serves(prof, media) {
					repaired = append(repaired, r)
					continue
				}
				rulesChanged = true
				if media != "" {
					if to := homeFor(media); to != "" {
						c := maps.Clone(r)
						c["profile"] = to
						repaired = append(repaired, c)
						notes = append(notes, fmt.Sprintf("quality.rules[%d].profile named a missing %s profile %q; now %q", i, media, prof, to))
					} else {
						notes = append(notes, fmt.Sprintf("quality.rules[%d].profile named a missing %s profile %q with no %s profiles; removed rule", i, media, prof, media))
					}
					continue
				}
				sharedTo := ""
				sameAcrossAll := true
				type mediaTarget struct{ media, to string }
				var targets []mediaTarget
				for _, m := range core.ProfileMediaTypes {
					to := homeFor(m)
					if to == "" {
						sameAcrossAll = false
						continue
					}
					targets = append(targets, mediaTarget{m, to})
					if sharedTo == "" {
						sharedTo = to
					} else if sharedTo != to {
						sameAcrossAll = false
					}
					if !serves(to, m) {
						sameAcrossAll = false
					}
				}
				switch {
				case sameAcrossAll && sharedTo != "":
					c := maps.Clone(r)
					c["profile"] = sharedTo
					repaired = append(repaired, c)
					notes = append(notes, fmt.Sprintf("quality.rules[%d].profile named a missing profile %q; now %q", i, prof, sharedTo))
				case len(targets) > 0:
					var parts []string
					for _, t := range targets {
						c := maps.Clone(r)
						c["mediaType"] = t.media
						c["profile"] = t.to
						repaired = append(repaired, c)
						parts = append(parts, fmt.Sprintf("%s=%q", t.media, t.to))
					}
					notes = append(notes, fmt.Sprintf("quality.rules[%d].profile named a missing profile %q; split to %s", i, prof, strings.Join(parts, ", ")))
				default:
					notes = append(notes, fmt.Sprintf("quality.rules[%d].profile named a missing profile %q with no profiles; removed rule", i, prof))
				}
			}
			if rulesChanged {
				quality["rules"] = repaired
			}
		}
	}
	return notes
}

func docString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// cloneProfile deep-copies p field by field so the per-media copies share
// no slices or maps. It does not go through JSON, whose decoding would fill
// defaults the original left unset.
func cloneProfile(p core.QualityProfile) core.QualityProfile {
	c := p
	c.ReleaseRules = nil
	for _, r := range p.ReleaseRules {
		r.Conditions = slices.Clone(r.Conditions)
		r.Unless = slices.Clone(r.Unless)
		r.Fallback = slices.Clone(r.Fallback)
		c.ReleaseRules = append(c.ReleaseRules, r)
	}
	if p.ReleaseRules != nil && c.ReleaseRules == nil {
		c.ReleaseRules = []core.ReleaseRule{}
	}
	if p.PluginScores != nil {
		c.PluginScores = make(map[string]json.RawMessage, len(p.PluginScores))
		for k, v := range p.PluginScores {
			c.PluginScores[k] = slices.Clone(v)
		}
	}
	c.QualityOrder = CloneQualityOrder(p.QualityOrder)
	c.AllowedQualities = slices.Clone(p.AllowedQualities)
	c.CustomFormatScores = maps.Clone(p.CustomFormatScores)
	if p.MinCustomFormatScore != nil {
		v := *p.MinCustomFormatScore
		c.MinCustomFormatScore = &v
	}
	if p.Import != nil {
		imp := *p.Import
		imp.Groups = slices.Clone(p.Import.Groups)
		c.Import = &imp
	}
	c.PreferredLanguages = slices.Clone(p.PreferredLanguages)
	return c
}

// CloneQualityOrder deep-copies a quality order.
func CloneQualityOrder(order []core.QualityItem) []core.QualityItem {
	if order == nil {
		return nil
	}
	out := make([]core.QualityItem, len(order))
	for i, it := range order {
		out[i] = core.QualityItem{Name: it.Name, Qualities: slices.Clone(it.Qualities)}
	}
	return out
}
