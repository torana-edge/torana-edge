package credentialcmd

import "testing"

func TestNormalizeSecretInputPreservesCredentialBytes(t *testing.T) {
	for _, test := range []struct {
		name string
		in   string
		want string
	}{
		{name: "terminal newline", in: "secret\n", want: "secret"},
		{name: "terminal crlf", in: "secret\r\n", want: "secret"},
		{name: "spaces", in: " secret \n", want: " secret "},
		{name: "tabs", in: "\tsecret\t\n", want: "\tsecret\t"},
		{name: "embedded newline", in: "first\nsecond\n", want: "first\nsecond"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeSecretInput([]byte(test.in)); got != test.want {
				t.Fatalf("normalizeSecretInput(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}
