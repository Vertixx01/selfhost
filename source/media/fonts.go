package media

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// ExtractedFont is one font attachment dumped to a temp dir, waiting to be
// registered in the font cache (utils.FontCache.Register).
type ExtractedFont struct {
	Index    int    `json:"index"`
	Filename string `json:"filename"` // sanitized original filename
	Path     string `json:"-"`        // temp file
}

// ExtractFonts dumps every font attachment into tmpDir. Failure is never
// fatal (PROJECT.MD Stage 4): it falls back to mkvextract, and if that fails
// too it returns no fonts and a warning — the ASS renderer uses system fonts.
func (t Tools) ExtractFonts(ctx context.Context, input string, sm *StreamMap, tmpDir string) ([]ExtractedFont, []string) {
	var fonts []ExtractedFont
	attachmentID := map[int]int{} // stream index → mkvextract attachment ID (1-based, all attachments)
	for i, a := range sm.Attachments {
		attachmentID[a.Index] = i + 1
		if a.Font {
			fonts = append(fonts, ExtractedFont{
				Index:    a.Index,
				Filename: safeFilename(a.Filename, a.Index),
				Path:     filepath.Join(tmpDir, "att"+strconv.Itoa(a.Index)),
			})
		}
	}
	if len(fonts) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, []string{"fonts: " + err.Error()}
	}

	// Dump to our own temp names: the attachment's filename comes from the
	// file and must not choose where we write.
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	for _, f := range fonts {
		args = append(args, "-dump_attachment:"+strconv.Itoa(f.Index), f.Path)
	}
	args = append(args, "-i", input, "-t", "0", "-f", "null", "-")
	ffErr := func() error { _, err := run(ctx, t.FFmpeg, args...); return err }()
	if ffErr == nil && allExist(fonts) {
		return fonts, nil
	}

	if t.Mkvextract != "" {
		margs := []string{input, "attachments"}
		for _, f := range fonts {
			margs = append(margs, fmt.Sprintf("%d:%s", attachmentID[f.Index], f.Path))
		}
		if _, err := run(ctx, t.Mkvextract, margs...); err == nil && allExist(fonts) {
			return fonts, nil
		}
	}

	// Keep whatever did get written.
	var got []ExtractedFont
	for _, f := range fonts {
		if st, err := os.Stat(f.Path); err == nil && st.Size() > 0 {
			got = append(got, f)
		}
	}
	return got, []string{fmt.Sprintf("font extraction incomplete (%d of %d): %v", len(got), len(fonts), ffErr)}
}

func allExist(fonts []ExtractedFont) bool {
	for _, f := range fonts {
		if st, err := os.Stat(f.Path); err != nil || st.Size() == 0 {
			return false
		}
	}
	return true
}

// safeFilename keeps the base name and drops characters Windows or a shell
// would choke on; it falls back to font{index}.ttf.
func safeFilename(name string, index int) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/")) // slash-only: no drive-letter parsing
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(strings.TrimRight(name, ". "))
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return fmt.Sprintf("font%d.ttf", index)
	}
	return name
}
