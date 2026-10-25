package transcode

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Key is an episode's AES-128 key (§7f). The key file stays in the work meta
// dir for ffmpeg; the packager moves it to KEY_DIR, never into the output.
type Key struct {
	URI      string // what players fetch: {HLS_KEY_URI_BASE}/{anime_id}/{ep_id}.key
	KeyPath  string // 16 raw bytes
	InfoPath string // ffmpeg -hls_key_info_file
	IV       []byte
}

// ensureKey reuses metaDir/enc.key + enc.keyinfo when a previous attempt
// made them for the same URI (a CRF retry or crash resume must not re-key
// half the segments); otherwise it creates new ones.
func ensureKey(metaDir, uri string) (*Key, error) {
	k := &Key{URI: uri, KeyPath: filepath.Join(metaDir, "enc.key"), InfoPath: filepath.Join(metaDir, "enc.keyinfo")}
	if b, err := os.ReadFile(k.KeyPath); err == nil && len(b) == 16 {
		if info, err := os.ReadFile(k.InfoPath); err == nil {
			if u, iv, ok := parseKeyInfo(string(info)); ok && u == uri {
				k.IV = iv
				return k, nil
			}
		}
	}

	key, iv := make([]byte, 16), make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(k.KeyPath, key, 0o600); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(k.KeyPath)
	if err != nil {
		return nil, err
	}
	// Line 1: key URI for the playlist; 2: key file ffmpeg reads; 3: IV hex.
	info := fmt.Sprintf("%s\n%s\n%s\n", uri, filepath.ToSlash(abs), hex.EncodeToString(iv))
	if err := os.WriteFile(k.InfoPath, []byte(info), 0o600); err != nil {
		return nil, err
	}
	k.IV = iv
	return k, nil
}

func parseKeyInfo(info string) (uri string, iv []byte, ok bool) {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(info, "\r\n", "\n")), "\n")
	if len(lines) != 3 || lines[0] == "" || lines[1] == "" {
		return "", nil, false
	}
	iv, err := hex.DecodeString(lines[2])
	if err != nil || len(iv) != 16 {
		return "", nil, false
	}
	return lines[0], iv, true
}
