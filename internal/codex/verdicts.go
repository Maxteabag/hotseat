package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Verdicts record what the server last said about a stored credential.
//
// Without them the listing can only report that a refresh token is present in
// the file, which is not the same as the account working: OpenAI revokes a
// refresh token when the account signs in again or signs out, and the file on
// disk is unchanged by that. A profile can sit there looking healthy for days.
//
// A verdict is pinned to the exact credential it was observed for. Signing in
// again rewrites auth.json, the digest stops matching, and the verdict is
// discarded rather than shown stale. Nothing here holds token material: only a
// name, a status, a hash and a time.

// Verdict is the last observed state of one profile's credential.
type Verdict struct {
	// Status is "live" or "revoked".
	Status string `json:"status"`
	// Digest pins the verdict to the credential bytes it describes.
	Digest string `json:"digest"`
	// At is when it was observed, in unix seconds.
	At float64 `json:"at"`
}

// VerdictsFile holds the verdicts beside the profiles they describe.
func verdictsFile(profilesDir string) string { return filepath.Join(profilesDir, ".verdicts.json") }

func credentialDigest(authPath string) string {
	payload, err := os.ReadFile(authPath)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ReadVerdicts returns the recorded verdicts, keyed by profile name. A missing
// or damaged file is an empty map: this is a cache, never a source of truth.
func ReadVerdicts(profilesDir string) map[string]Verdict {
	out := map[string]Verdict{}
	raw, err := os.ReadFile(verdictsFile(profilesDir))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

// VerdictFor returns the verdict for name when it still describes the
// credential currently on disk, and false when there is none or it is stale.
func VerdictFor(profilesDir, name, authPath string) (Verdict, bool) {
	verdict, ok := ReadVerdicts(profilesDir)[name]
	if !ok || verdict.Digest == "" || verdict.Digest != credentialDigest(authPath) {
		return Verdict{}, false
	}
	return verdict, true
}

// RecordVerdict stores what the server said about name's credential.
//
// Failing to record is never an error for the caller: the verdict only makes
// the listing more honest, and losing it costs nothing but a re-probe.
func RecordVerdict(profilesDir, name, status, authPath string) {
	digest := credentialDigest(authPath)
	if digest == "" || name == "" || name == LiveName {
		return
	}
	verdicts := ReadVerdicts(profilesDir)
	verdicts[name] = Verdict{Status: status, Digest: digest, At: float64(time.Now().Unix())}
	// Drop entries for profiles that no longer exist, so the file cannot grow
	// without bound as profiles come and go.
	for key := range verdicts {
		if !fileExists(filepath.Join(profilesDir, key, "auth.json")) {
			delete(verdicts, key)
		}
	}
	keys := make([]string, 0, len(verdicts))
	for key := range verdicts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ordered := make(map[string]Verdict, len(keys))
	for _, key := range keys {
		ordered[key] = verdicts[key]
	}
	payload, err := json.MarshalIndent(ordered, "", " ")
	if err != nil {
		return
	}
	_ = atomicWrite(verdictsFile(profilesDir), payload)
}
