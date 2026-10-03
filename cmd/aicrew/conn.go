package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/BlackVS/aicrew/internal/opclient"
	"github.com/BlackVS/aicrew/internal/tlstrust"
)

// Every administrative command reaches aicrewd's operator API. Its flags
// fall back to the environment, so an operator sets them once per shell.
const connUsage = `connection (each flag falls back to its environment variable):
  -url https://HOST[:PORT]        AICREW_URL             aicrewd's origin
  -tls-trust-mode ca_dns|spki_sha256  AICREW_TLS_TRUST_MODE
  -tls-trust-value VALUE          AICREW_TLS_TRUST_VALUE  the host name, or sha256- and the pin
  -token-file PATH                AICREW_OPERATOR_TOKEN_FILE  the operator credential's owner-only file
`

// connFlags are the connection's flags.
var connFlags = []string{"url", "tls-trust-mode", "tls-trust-value", "token-file"}

type conn struct {
	url, mode, value, tokenFile *string
}

func addConn(fs *flag.FlagSet) *conn {
	return &conn{
		url:       fs.String("url", "", "aicrewd's https origin"),
		mode:      fs.String("tls-trust-mode", "", "ca_dns or spki_sha256"),
		value:     fs.String("tls-trust-value", "", "the host name, or sha256- and the pin"),
		tokenFile: fs.String("token-file", "", "the operator credential's owner-only file"),
	}
}

func orEnv(v *string, name string) string {
	if *v != "" {
		return *v
	}
	return os.Getenv(name)
}

// client builds the operator API's client, or explains what is missing.
func (c *conn) client() (*opclient.Client, error) {
	cfg := opclient.Config{
		URL:       orEnv(c.url, "AICREW_URL"),
		Trust:     tlstrust.Binding{Mode: orEnv(c.mode, "AICREW_TLS_TRUST_MODE"), Value: orEnv(c.value, "AICREW_TLS_TRUST_VALUE")},
		TokenFile: orEnv(c.tokenFile, "AICREW_OPERATOR_TOKEN_FILE"),
	}
	if cfg.URL == "" || cfg.Trust.Mode == "" || cfg.Trust.Value == "" || cfg.TokenFile == "" {
		return nil, errors.New("name aicrewd and the operator credential: -url, -tls-trust-mode, -tls-trust-value and -token-file, or their environment variables")
	}
	return opclient.New(cfg)
}

// connect builds the client or reports why not; ok is false on a failure.
func (c *conn) connect(stderr io.Writer) (*opclient.Client, bool) {
	cl, err := c.client()
	if err != nil {
		fmt.Fprintln(stderr, "aicrew:", err)
		return nil, false
	}
	return cl, true
}

// failed reports a refused or failed call and returns exit 1.
func failed(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "aicrew:", err)
	return 1
}

// allowedFlags reports whether set holds only the connection's flags and
// allowed.
func allowedFlags(set map[string]bool, allowed ...string) bool {
	ok := map[string]bool{}
	for _, a := range append(append([]string{}, connFlags...), allowed...) {
		ok[a] = true
	}
	for name := range set {
		if !ok[name] {
			return false
		}
	}
	return true
}
