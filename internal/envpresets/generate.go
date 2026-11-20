package envpresets

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
)

// djangoAlphabet leaves out the symbols of Django's own generator so that
// the value needs no quoting anywhere.
const djangoAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// Generate makes a secret value in the format of the framework's own
// generator, reading randomness from random (crypto/rand.Reader outside of
// tests). The value must never be logged.
func Generate(generator string, random io.Reader) (string, error) {
	switch generator {
	case GenRailsSecretKeyBase:
		// bin/rails secret: 64 random bytes in hex.
		b, err := randomBytes(random, 64)
		return hex.EncodeToString(b), err
	case GenPhoenixSecretKeyBase:
		// mix phx.gen.secret: 64 characters of base64.
		b, err := randomBytes(random, 48)
		return base64.StdEncoding.EncodeToString(b), err
	case GenLaravelAppKey:
		// php artisan key:generate: 32 bytes for AES-256-CBC.
		b, err := randomBytes(random, 32)
		return "base64:" + base64.StdEncoding.EncodeToString(b), err
	case GenDjangoSecretKey:
		// get_random_secret_key(): 50 characters.
		return randomString(random, djangoAlphabet, 50)
	}
	return "", fmt.Errorf("envpresets: unknown generator %q", generator)
}

func randomBytes(random io.Reader, n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(random, b); err != nil {
		return nil, fmt.Errorf("envpresets: reading random bytes: %w", err)
	}
	return b, nil
}

// randomString picks n characters uniformly from alphabet, rejecting the
// bytes that would bias the choice.
func randomString(random io.Reader, alphabet string, n int) (string, error) {
	limit := 256 - 256%len(alphabet)
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := io.ReadFull(random, buf); err != nil {
			return "", fmt.Errorf("envpresets: reading random bytes: %w", err)
		}
		for _, b := range buf {
			if int(b) < limit && len(out) < n {
				out = append(out, alphabet[int(b)%len(alphabet)])
			}
		}
	}
	return string(out), nil
}
