package crypto

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func testKey() Key {
	var k Key
	for i := range k.Key {
		k.Key[i] = byte(i)
		k.IV[i] = byte(0xf0 + i)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	k := testKey()
	for _, n := range []int{0, 1, 15, 16, 17, 1000} {
		plain := bytes.Repeat([]byte{0xab}, n)
		enc := k.Encrypt(plain)
		if len(enc)%16 != 0 || len(enc) <= n {
			t.Fatalf("n=%d：密文长度 %d", n, len(enc))
		}
		dec, err := k.Decrypt(enc)
		if err != nil || !bytes.Equal(dec, plain) {
			t.Fatalf("n=%d：往返失败 %v", n, err)
		}
	}
}

func TestWrongKeyIsPaddingError(t *testing.T) {
	enc := testKey().Encrypt([]byte("hello"))
	other := testKey()
	other.Key[0] ^= 1
	if _, err := other.Decrypt(enc); !errors.Is(err, ErrPadding) {
		t.Errorf("用错密钥应报填充错误，得到 %v", err)
	}
	if _, err := testKey().Decrypt([]byte{1, 2, 3}); err == nil {
		t.Error("长度不是 16 的倍数应报错")
	}
}

func TestParseBits(t *testing.T) {
	s := "00010010" + strings.Repeat("0", 112) + "11111111"
	b, err := ParseBits(s)
	if err != nil {
		t.Fatal(err)
	}
	if b[0] != 0x12 || b[15] != 0xff || b[1] != 0 {
		t.Errorf("得到 % x", b)
	}
	if _, err := ParseBits(s[:127]); err == nil {
		t.Error("长度不对应报错")
	}
	if _, err := ParseBits(strings.Repeat("2", 128)); err == nil {
		t.Error("非 0/1 字符应报错")
	}
}
