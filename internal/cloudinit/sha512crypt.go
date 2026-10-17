package cloudinit

import (
	"crypto/rand"
	"crypto/sha512"
	"strconv"
)

// The SHA-512 crypt scheme ($6$) that every Linux shadow file understands.
// cloud-init writes it straight into /etc/shadow, so the password itself
// never goes onto the seed disk. Specification:
// https://www.akkadia.org/drepper/SHA-crypt.txt

const (
	cryptB64   = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	hashRounds = 100000 // the default (5000) is cheap to brute-force; logins stay fast
)

// HashPassword returns a $6$ hash of password with a random salt.
func HashPassword(password string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	salt := make([]byte, 16)
	for i, b := range raw {
		salt[i] = cryptB64[b&0x3f]
	}
	return sha512Crypt([]byte(password), salt, hashRounds, true), nil
}

func sha512Crypt(key, salt []byte, rounds int, explicit bool) string {
	if len(salt) > 16 {
		salt = salt[:16]
	}
	rounds = min(max(rounds, 1000), 999999999)

	b := sha512.New()
	b.Write(key)
	b.Write(salt)
	b.Write(key)
	bsum := b.Sum(nil)

	a := sha512.New()
	a.Write(key)
	a.Write(salt)
	n := len(key)
	for ; n > 64; n -= 64 {
		a.Write(bsum)
	}
	a.Write(bsum[:n])
	for n = len(key); n > 0; n >>= 1 {
		if n&1 != 0 {
			a.Write(bsum)
		} else {
			a.Write(key)
		}
	}
	asum := a.Sum(nil)

	dp := sha512.New()
	for range len(key) {
		dp.Write(key)
	}
	p := repeatTo(dp.Sum(nil), len(key))

	ds := sha512.New()
	for range 16 + int(asum[0]) {
		ds.Write(salt)
	}
	s := repeatTo(ds.Sum(nil), len(salt))

	c := asum
	for r := range rounds {
		h := sha512.New()
		if r&1 != 0 {
			h.Write(p)
		} else {
			h.Write(c)
		}
		if r%3 != 0 {
			h.Write(s)
		}
		if r%7 != 0 {
			h.Write(p)
		}
		if r&1 != 0 {
			h.Write(c)
		} else {
			h.Write(p)
		}
		c = h.Sum(nil)
	}

	out := []byte("$6$")
	if explicit {
		out = append(out, "rounds="+strconv.Itoa(rounds)+"$"...)
	}
	out = append(out, salt...)
	out = append(out, '$')
	order := [][3]int{
		{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4}, {47, 5, 26}, {6, 27, 48},
		{28, 49, 7}, {50, 8, 29}, {9, 30, 51}, {31, 52, 10}, {53, 11, 32}, {12, 33, 54}, {34, 55, 13},
		{56, 14, 35}, {15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19}, {62, 20, 41},
	}
	for _, o := range order {
		out = b64From24(out, c[o[0]], c[o[1]], c[o[2]], 4)
	}
	out = b64From24(out, 0, 0, c[63], 2)
	return string(out)
}

func repeatTo(sum []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out)+len(sum) <= n {
		out = append(out, sum...)
	}
	return append(out, sum[:n-len(out)]...)
}

func b64From24(out []byte, b2, b1, b0 byte, n int) []byte {
	w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
	for range n {
		out = append(out, cryptB64[w&0x3f])
		w >>= 6
	}
	return out
}
