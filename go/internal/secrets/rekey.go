package secrets

import "context"

// Reseal re-wraps one stored value under the ring's current key. A value
// already under the current key comes back unchanged, so the caller can
// run over every row without first asking which ones need it.
func Reseal(ctx context.Context, ring Keyring, blob []byte, keyVersion, aad string) ([]byte, string, error) {
	if keyVersion == ring.CurrentVersion() {
		return blob, keyVersion, nil
	}
	plain, err := ring.Open(ctx, blob, keyVersion, aad)
	if err != nil {
		return nil, "", err
	}
	return ring.Seal(ctx, plain, aad)
}
