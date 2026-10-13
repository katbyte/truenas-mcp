package cli

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"github.com/katbyte/go-kt/clog"
	"github.com/katbyte/go-kt/version"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type FlagData struct {
	Server               string `mapstructure:"server"`
	Username             string `mapstructure:"username"`
	Token                string `mapstructure:"token"`
	CertificateFile      string `mapstructure:"certificate-file"`
	CAFile               string `mapstructure:"ca-file"`
	SkipCertificateCheck bool   `mapstructure:"skip-certificate-check"`
	TokenFile            string `mapstructure:"token-file"`
	SendKeyUnchecked     bool   `mapstructure:"send-key-unchecked"`
	NoChannelBinding     bool   `mapstructure:"no-channel-binding"`
}

func configureFlags(root *cobra.Command) error {
	pflags := root.PersistentFlags()

	pflags.StringP("server", "s", "", "the TrueNAS's address, e.g. nas.example.net or https://nas.example.net:8443, or unix:// when run on the NAS itself, which needs no key")
	pflags.StringP("username", "u", "", "the user the API key was made for (without it the key is logged in the older way, which TrueNAS 27 takes away)")
	pflags.StringP("token", "t", "", "the TrueNAS API key (consider exporting to TRUENAS_TOKEN instead)")
	pflags.String("certificate-file", "", "the TrueNAS's own certificate as a PEM file, accepted whatever name is on it: for a NAS still on the certificate it came with")
	pflags.String("ca-file", "", "a PEM file of the certificate authority that signed the TrueNAS's certificate, for a NAS given one made out to its name")
	pflags.Bool("skip-certificate-check", false, "take whatever certificate the TrueNAS shows: nothing then says it is the NAS that answers, and a TrueNAS before version 26 is not sent the key unless --send-key-unchecked says so")
	pflags.String("token-file", "", "a file holding the TrueNAS API key, or a key file as TrueNAS hands one out, in place of --token")
	pflags.Bool("send-key-unchecked", false, "with --skip-certificate-check, send the key to a TrueNAS before version 26 all the same: it takes a key only as it is, so whatever answers at the address is handed it")
	pflags.Bool("no-channel-binding", false, "do not tie the login to the certificate the TrueNAS shows: for a TrueNAS 26 or later reached through a reverse proxy, which shows a certificate of its own")

	// binding map for viper/pflag -> env
	m := map[string]string{ //nolint:gosec // G101: these are the names of settings, not what they are set to
		"server":                 "TRUENAS_SERVER",
		"username":               "TRUENAS_USERNAME",
		"token":                  "TRUENAS_TOKEN",
		"certificate-file":       "TRUENAS_CERTIFICATE_FILE",
		"ca-file":                "TRUENAS_CA_FILE",
		"skip-certificate-check": "TRUENAS_SKIP_CERTIFICATE_CHECK",
		"token-file":             "TRUENAS_TOKEN_FILE",
		"send-key-unchecked":     "TRUENAS_SEND_KEY_UNCHECKED",
		"no-channel-binding":     "TRUENAS_NO_CHANNEL_BINDING",
	}

	for name, env := range m {
		if err := viper.BindPFlag(name, pflags.Lookup(name)); err != nil {
			return fmt.Errorf("error binding '%s' flag: %w", name, err)
		}

		if err := viper.BindEnv(name, env); err != nil {
			return fmt.Errorf("error binding '%s' to env '%s' : %w", name, env, err)
		}
	}

	viper.SetConfigName(".truenas-mcp")
	viper.SetConfigType("env")
	// viper reads the first file it finds, so the working directory comes first: a per-project .truenas-mcp overrides the one in $HOME
	viper.AddConfigPath(".")
	if home, err := os.UserHomeDir(); err == nil {
		viper.AddConfigPath(home)
	}

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := errors.AsType[viper.ConfigFileNotFoundError](err); !ok {
			clog.Log.Errorf("Error reading config file: %v", err)
		}
	}

	return nil
}

// GetFlags returns the fully populated FlagData. It is read from viper, not from the flags themselves, because the flags hold only what was typed on
// the command line and viper lays the environment and the config file over that.
func GetFlags() *FlagData {
	var f FlagData
	if err := viper.Unmarshal(&f); err != nil {
		clog.Log.Fatalf("failed to unmarshal configuration: %v", err)
	}

	return &f
}

// NewConn is a connection to the configured TrueNAS, which says what it does in the process's log at the level TRUENAS_LOG sets, with the key hidden.
// Nothing is dialled until it is first called on.
func (f *FlagData) NewConn() (*truenasrpc.Conn, error) {
	key := f.Token
	if f.TokenFile != "" {
		if key != "" {
			return nil, errors.New("the TrueNAS API key is given twice, as token and as token-file: give one")
		}

		text, err := os.ReadFile(f.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading the TrueNAS API key file: %w", err)
		}
		key = string(text)
	}

	var how []truenasrpc.LoginOption
	if f.SendKeyUnchecked {
		how = append(how, truenasrpc.EvenUnchecked())
	}
	if f.NoChannelBinding {
		how = append(how, truenasrpc.WithoutChannelBinding())
	}

	cred := truenasrpc.APIKey(f.Username, key, how...)
	switch {
	case key == "" && truenasrpc.OverSocket(f.Server):
		// on the NAS itself, over its own socket, the NAS can see who is asking
		cred = truenasrpc.LocalUser()
	case f.Username == "":
		cred = truenasrpc.APIKeyOnly(key, how...)
	}

	opts := []truenasrpc.Option{truenasrpc.WithLog(clog.Log), truenasrpc.WithUserAgent("truenas-mcp/" + version.Version)}
	if f.CertificateFile != "" {
		opts = append(opts, truenasrpc.WithCertificateFile(f.CertificateFile))
	}
	if f.CAFile != "" {
		opts = append(opts, truenasrpc.WithCAFile(f.CAFile))
	}
	if f.SkipCertificateCheck {
		opts = append(opts, truenasrpc.WithoutCertificateCheck())
	}

	return truenasrpc.New(f.Server, cred, opts...)
}

// hinted is a connection's error with the flags that answer it added, where there are any: the connection says what to do in its own words, and a
// person at a command line needs the names of the flags.
func hinted(err error) error {
	login, refused := errors.AsType[*truenasrpc.LoginError](err)
	_, otherName := errors.AsType[x509.HostnameError](err)
	_, untrusted := errors.AsType[*tls.CertificateVerificationError](err)

	switch {
	case errors.Is(err, truenasrpc.ErrNothingVouches):
		return fmt.Errorf("%w (--certificate-file hands over the certificate, --send-key-unchecked sends the key anyway)", err)
	case refused && login.Tied:
		return fmt.Errorf("%w (--no-channel-binding leaves the login untied)", err)
	case otherName:
		return fmt.Errorf("%w (--certificate-file)", err)
	case untrusted:
		return fmt.Errorf("%w (--certificate-file, --ca-file or --skip-certificate-check)", err)
	default:
		return err
	}
}
