package dnsproviders

import (
	"fmt"
	"net/http"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
)

type cloudflareGlobalKeyClient struct {
	email  string
	key    string
	client cloudflare.HTTPClient
}

func (c *cloudflareGlobalKeyClient) Do(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header = req.Header.Clone()
	cloned.Header.Del("Authorization")
	cloned.Header.Set("X-Auth-Email", c.email)
	cloned.Header.Set("X-Auth-Key", c.key)
	return c.client.Do(cloned)
}

func init() {
	Register(&Provider{
		Names:   []string{"cloudflare", "cf"},
		EnvVars: []string{"CLOUDFLARE_DNS_API_TOKEN", "CF_API_TOKEN", "CLOUDFLARE_API_TOKEN", "CLOUDFLARE_API_KEY", "CLOUDFLARE_EMAIL"},
		Build: func(env map[string]string) (certmagic.DNSProvider, error) {
			token := firstOf(env, "CLOUDFLARE_DNS_API_TOKEN", "CF_API_TOKEN", "CLOUDFLARE_API_TOKEN")
			if token != "" {
				return &cloudflare.Provider{APIToken: token}, nil
			}
			key := firstOf(env, "CLOUDFLARE_API_KEY", "CF_API_KEY")
			email := firstOf(env, "CLOUDFLARE_EMAIL", "CF_API_EMAIL")
			if key == "" || email == "" {
				return nil, fmt.Errorf("cloudflare requires an API token or CLOUDFLARE_EMAIL+CLOUDFLARE_API_KEY in dns_env")
			}
			return &cloudflare.Provider{
				// libdns/cloudflare requires this field to be non-empty before it
				// delegates the request. The custom client replaces the bearer
				// header with Cloudflare's legacy global-key headers.
				APIToken: key,
				HTTPClient: &cloudflareGlobalKeyClient{
					email:  email,
					key:    key,
					client: http.DefaultClient,
				},
			}, nil
		},
	})
}
