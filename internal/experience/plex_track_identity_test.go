package experience

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

// referencePlexPlaySelection transcribes plex resolveSelection and
// resolvePlaySelection. It is independent of TranslatePlexTrackIdentity's
// control flow. Audio set-ness is the nonnegative position, and a nil
// identity is left untouched, including invalid positions. Subtitle omission
// forces position -1 after resolution.
func referencePlexPlaySelection(issued IssuedTrackIdentity, info core.MediaInfo) TrackSelection {
	audio, audioSet := issued.AudioPosition, issued.AudioPosition >= 0
	sub, subSet := issued.SubtitlePosition, issued.SubtitleSet
	if audioSet && issued.Audio != nil {
		audio = ResolveAudio(issued.Audio, audio, info)
		audioSet = audio >= 0
	}
	if subSet && issued.Subtitle != nil {
		if pos := ResolveSubtitle(issued.Subtitle, sub, info); pos >= 0 {
			sub = pos
		} else {
			sub, subSet = -1, false
		}
	}
	if !subSet {
		sub = -1
	}
	return TrackSelection{Audio: audio, AudioSet: audioSet, Subtitle: sub, SubtitleSet: subSet}
}

func shiftedProbe() core.MediaInfo {
	return core.MediaInfo{Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264"},
		{Index: 4, Type: "audio", Language: "fre", Codec: "ac3", Channels: 6, Title: "Main"},
		{Index: 7, Type: "subtitle", Language: "eng", Codec: "subrip"},
		{Index: 11, Type: "audio", Language: "eng", Codec: "ac3", Channels: 6, Title: "Main"},
		{Index: 20, Type: "subtitle", Language: "eng", External: true, ExternalURL: "sidecar://en"},
		{Index: 21, Type: "subtitle", Language: "eng", Forced: true},
	}}
}

func englishAudio() core.StreamInfo {
	return core.StreamInfo{Type: "audio", Language: "eng", Codec: "ac3", Channels: 6, Title: "Main"}
}

func issuedAudio(stream *core.StreamInfo, pos int, subPos int, subSet bool, sub *core.StreamInfo) IssuedTrackIdentity {
	return IssuedTrackIdentity{
		Audio: stream, AudioPosition: pos, AudioSet: pos >= 0,
		Subtitle: sub, SubtitlePosition: subPos, SubtitleSet: subSet,
	}
}

func TestTranslatePlexTrackIdentityMatchesPlaySelection(t *testing.T) {
	info := shiftedProbe()
	english := englishAudio()
	sidecar := core.StreamInfo{Type: "subtitle", Language: "fr", External: true, ExternalURL: "sidecar://en"}
	cases := []struct {
		name   string
		issued IssuedTrackIdentity
		want   TrackSelection
	}{
		{
			name:   "issued audio moves off the old position",
			issued: issuedAudio(&english, 1, -1, false, nil),
			want:   TrackSelection{Audio: 3, AudioSet: true, Subtitle: -1},
		},
		{
			name:   "unmatched audio identity clears the set flag",
			issued: issuedAudio(&core.StreamInfo{Type: "audio", Language: "deu", Codec: "aac"}, 1, -1, false, nil),
			want:   TrackSelection{Audio: -1, Subtitle: -1},
		},
		{
			name:   "legacy audio on a subtitle slot stays set",
			issued: issuedAudio(nil, 2, -1, false, nil),
			want:   TrackSelection{Audio: 2, AudioSet: true, Subtitle: -1},
		},
		{
			name:   "legacy audio past the end stays set",
			issued: issuedAudio(nil, 40, -1, false, nil),
			want:   TrackSelection{Audio: 40, AudioSet: true, Subtitle: -1},
		},
		{
			name:   "negative audio omission is preserved",
			issued: issuedAudio(nil, -4, -1, false, nil),
			want:   TrackSelection{Audio: -4, Subtitle: -1},
		},
		{
			name:   "explicit subtitle off is -1 and set",
			issued: issuedAudio(nil, -1, -1, true, nil),
			want:   TrackSelection{Audio: -1, Subtitle: -1, SubtitleSet: true},
		},
		{
			name:   "legacy subtitle on an audio slot stays set",
			issued: issuedAudio(nil, -1, 1, true, nil),
			want:   TrackSelection{Audio: -1, Subtitle: 1, SubtitleSet: true},
		},
		{
			name:   "legacy subtitle past the end stays set",
			issued: issuedAudio(nil, -1, 40, true, nil),
			want:   TrackSelection{Audio: -1, Subtitle: 40, SubtitleSet: true},
		},
		{
			name:   "other negative legacy subtitle is not rewritten to off",
			issued: issuedAudio(nil, -1, -5, true, nil),
			want:   TrackSelection{Audio: -1, Subtitle: -5, SubtitleSet: true},
		},
		{
			name:   "subtitle omission forces -1",
			issued: issuedAudio(&english, 1, 2, false, &sidecar),
			want:   TrackSelection{Audio: 3, AudioSet: true, Subtitle: -1},
		},
		{
			name:   "sidecar url wins over an earlier embedded subtitle",
			issued: issuedAudio(nil, -1, 2, true, &sidecar),
			want:   TrackSelection{Audio: -1, Subtitle: 4, SubtitleSet: true},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := TranslatePlexTrackIdentity(tt.issued, info)
			ref := referencePlexPlaySelection(tt.issued, info)
			if err != nil || got != tt.want || got != ref {
				t.Fatalf("got %+v err %v, want %+v, reference %+v", got, err, tt.want, ref)
			}
		})
	}
	if info.Streams[3].Index != 11 || info.Streams[4].Index != 20 {
		t.Fatal("fixture indexes were contiguous")
	}
}

func TestTranslatePlexTrackIdentityWrongTypeUsesExistingResolvers(t *testing.T) {
	info := shiftedProbe()
	audioShaped := core.StreamInfo{Type: "subtitle", Language: "eng", Codec: "ac3", Channels: 6, Title: "Main"}
	if pos := ResolveAudio(&audioShaped, 1, info); pos != 3 {
		t.Fatalf("ResolveAudio wrong type = %d, want 3", pos)
	}
	got, err := TranslatePlexTrackIdentity(issuedAudio(&audioShaped, 1, -1, false, nil), info)
	if err != nil || got.Audio != 3 || !got.AudioSet {
		t.Fatalf("wrong-type audio identity = %+v, %v", got, err)
	}
	blank := core.StreamInfo{Type: "video", Codec: "ac3", Channels: 6, Title: "Main"}
	if pos := ResolveAudio(&blank, 3, info); pos != -1 {
		t.Fatalf("language-free type mismatch = %d, want -1", pos)
	}
	got, err = TranslatePlexTrackIdentity(issuedAudio(&blank, 3, -1, false, nil), info)
	if err != nil || got.Audio != -1 || got.AudioSet || got != referencePlexPlaySelection(issuedAudio(&blank, 3, -1, false, nil), info) {
		t.Fatalf("wrong-type language-free audio = %+v, %v", got, err)
	}

	subtitleShaped := core.StreamInfo{Type: "audio", Language: "eng"}
	if pos := ResolveSubtitle(&subtitleShaped, 1, info); pos != 2 {
		t.Fatalf("ResolveSubtitle wrong type = %d, want 2", pos)
	}
	got, err = TranslatePlexTrackIdentity(issuedAudio(nil, -1, 1, true, &subtitleShaped), info)
	if err != nil || got.Subtitle != 2 || !got.SubtitleSet {
		t.Fatalf("wrong-type subtitle identity = %+v, %v", got, err)
	}
	external := core.StreamInfo{Type: "audio", External: true, ExternalURL: "sidecar://en"}
	if pos := ResolveSubtitle(&external, -1, info); pos != 4 {
		t.Fatalf("ResolveSubtitle wrong-type sidecar = %d, want 4", pos)
	}
	got, err = TranslatePlexTrackIdentity(issuedAudio(nil, -1, -1, true, &external), info)
	if err != nil || got.Subtitle != 4 || !got.SubtitleSet || info.Streams[got.Subtitle].Index != 20 {
		t.Fatalf("wrong-type sidecar = %+v, %v", got, err)
	}
	unmatched := core.StreamInfo{Type: "audio", Language: ""}
	if pos := ResolveSubtitle(&unmatched, 1, info); pos != -1 {
		t.Fatalf("ResolveSubtitle empty wrong type = %d, want -1", pos)
	}
	got, err = TranslatePlexTrackIdentity(issuedAudio(nil, -1, 1, true, &unmatched), info)
	if err != nil || got.Subtitle != -1 || got.SubtitleSet {
		t.Fatalf("unmatched wrong-type subtitle = %+v, %v", got, err)
	}
}

func TestTranslatePlexTrackIdentityTieMatchesResolver(t *testing.T) {
	info := core.MediaInfo{Streams: []core.StreamInfo{
		{Index: 0, Type: "video"},
		{Index: 4, Type: "audio", Language: "eng", Codec: "aac", Channels: 2, Title: "A"},
		{Index: 8, Type: "audio", Language: "eng", Codec: "aac", Channels: 2, Title: "A"},
		{Index: 1, Type: "subtitle", Language: "eng"},
		{Index: 7, Type: "subtitle", Language: "eng"},
	}}
	wantAudio := info.Streams[1]
	later := info.Streams[4]
	issued := issuedAudio(&wantAudio, 0, 0, true, &later)
	got, err := TranslatePlexTrackIdentity(issued, info)
	if err != nil || got != referencePlexPlaySelection(issued, info) {
		t.Fatalf("tie = %+v, reference %+v, err %v", got, referencePlexPlaySelection(issued, info), err)
	}
	if got.Audio != ResolveAudio(&wantAudio, 0, info) || got.Subtitle != ResolveSubtitle(&later, 0, info) {
		t.Fatalf("resolver disagreement: %+v", got)
	}
	// Equal scores keep the earlier stream. The stored subtitle index is not
	// searched across the slice once the old position no longer has it.
	if got.Audio != 1 || got.Subtitle != 3 || info.Streams[got.Subtitle].Index != 1 {
		t.Fatalf("tie positions = %+v", got)
	}
	issued.AudioPosition, issued.AudioSet = 2, true
	issued.Subtitle, issued.SubtitlePosition = &info.Streams[4], 4
	got, err = TranslatePlexTrackIdentity(issued, info)
	if err != nil || got.Audio != 2 || got.Subtitle != 4 || info.Streams[got.Subtitle].Index != 7 {
		t.Fatalf("position and embedded index = %+v %v", got, err)
	}
}

func TestTranslatePlexTrackIdentityLegacyPreferencesAndUnsupported(t *testing.T) {
	ctx := context.Background()
	info := shiftedProbe()
	fake := &fakeTrackPreferences{selection: trackprefs.Selection{Audio: 3, Subtitle: 4}}
	prefs := TrackPreferences{Source: fake}
	viewer := &core.User{ID: 1}
	key := core.MovieKey(1)

	legacy, err := TranslatePlexTrackIdentity(issuedAudio(nil, 40, 1, true, nil), info)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := prefs.Select(ctx, viewer, key, info, "en", legacy)
	if err != nil || selected != legacy || !selected.AudioSet || selected.Audio != 40 || !selected.SubtitleSet || selected.Subtitle != 1 {
		t.Fatalf("preferences changed invalid legacy selection: %+v %v", selected, err)
	}

	off, err := TranslatePlexTrackIdentity(issuedAudio(nil, -1, -1, true, nil), info)
	if err != nil || off.Subtitle != -1 || !off.SubtitleSet {
		t.Fatalf("explicit off = %+v %v", off, err)
	}
	selected, err = prefs.Select(ctx, viewer, key, info, "en", off)
	if err != nil || selected.Subtitle != -1 || !selected.SubtitleSet || selected.Audio != 3 || !selected.AudioSet {
		t.Fatalf("explicit off lost or audio preference skipped: %+v %v", selected, err)
	}

	missing := core.StreamInfo{Type: "audio", Language: "deu"}
	unmatched, err := TranslatePlexTrackIdentity(issuedAudio(&missing, 1, 2, false, nil), info)
	if err != nil || unmatched.AudioSet || unmatched.SubtitleSet {
		t.Fatalf("unmatched identity = %+v %v", unmatched, err)
	}
	selected, err = prefs.Select(ctx, viewer, key, info, "en", unmatched)
	if err != nil || selected.Audio != 3 || !selected.AudioSet || selected.Subtitle != 4 || !selected.SubtitleSet {
		t.Fatalf("preference fallback = %+v %v", selected, err)
	}

	_, err = TranslatePlexTrackIdentity(IssuedTrackIdentity{AudioPosition: -1, AudioSet: true, SubtitlePosition: -1}, info)
	var unsupported UnsupportedPlexTrackIdentityError
	if !errors.As(err, &unsupported) || !errors.Is(err, UnsupportedPlexTrackIdentityError{}) {
		t.Fatalf("disagreed audio set = %v", err)
	}
	got, err := TranslatePlexTrackIdentity(IssuedTrackIdentity{}, info)
	if !errors.Is(err, unsupported) || got != (TrackSelection{}) {
		t.Fatalf("zero value = %+v %v", got, err)
	}
}
