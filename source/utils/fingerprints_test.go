package utils

import (
	"bytes"
	"context"
	"testing"
)

func TestFingerprintStore(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, ep := range []string{"S01E01", "S01E02"} {
		if err := s.SaveFingerprint(ctx, "show", ep, "intro", 0, []byte{1, 2, 3, 4}); err != nil {
			t.Fatal(err)
		}
	}
	// Re-processing replaces, never duplicates.
	if err := s.SaveFingerprint(ctx, "show", "S01E02", "intro", 0, []byte{9, 9, 9, 9}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveFingerprint(ctx, "show", "S01E01", "outro", 840.5, []byte{5, 6, 7, 8}); err != nil {
		t.Fatal(err)
	}

	if n, err := s.FingerprintedEpisodes(ctx, "show"); err != nil || n != 2 {
		t.Fatalf("FingerprintedEpisodes = %d, %v", n, err)
	}
	intro, err := s.Fingerprints(ctx, "show", "intro")
	if err != nil || len(intro) != 2 || !bytes.Equal(intro[1].Data, []byte{9, 9, 9, 9}) {
		t.Fatalf("intro rows %+v %v", intro, err)
	}
	outro, err := s.Fingerprints(ctx, "show", "outro")
	if err != nil || len(outro) != 1 || outro[0].Offset != 840.5 {
		t.Fatalf("outro rows %+v %v", outro, err)
	}
}
