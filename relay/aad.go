package relay

// AAD construction for AEAD body encryption.
//
// Spec: AAD = UTF-8 string bytes of path_id concatenated with UTF-8
// string bytes of msg_id, no separator, no length prefix. Both values
// are ASCII-safe in their canonical form (path_id = "nxc_<base64url>",
// msg_id = UUIDv7), so UTF-8 == raw string bytes.
//
// This binding means a ciphertext extracted from one envelope cannot
// be successfully decrypted against a different path_id or msg_id —
// AEAD authentication fails at decrypt time. The relay cannot replay
// or substitute envelopes across paths even at the AEAD layer.
//
// MakeAAD is the single source of truth for this calculation: every
// call site (Put, recv, tests, future SDK consumers) MUST go through
// it. Inlining the calculation at multiple sites is exactly how the
// nil-vs-sha256(ciphertext) discrepancy happened — spec text and
// implementation diverged in places nobody cross-checked.
//
// Discovery doc reference:
// /.well-known/nexus-interchange → crypto.encryption.aad
// /.well-known/nexus-interchange → examples.aead_vector (verifiable)
func MakeAAD(pathID, msgID string) []byte {
	return []byte(pathID + msgID)
}
