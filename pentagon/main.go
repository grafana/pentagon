package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/hashicorp/vault/api"
	yaml "gopkg.in/yaml.v2"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/vimeo/pentagon"
	"github.com/vimeo/pentagon/vault"
)

var successGauge = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "pentagon_status",
	Help: "Status of the last attempt to reflect secrets. 1 for success, 0 for failure",
})

func main() {
	if len(os.Args) != 2 {
		log.Printf(
			"incorrect number of arguments. need 2, got %d [%#v]",
			len(os.Args),
			os.Args,
		)
		os.Exit(10)
	}

	configFile, err := ioutil.ReadFile(os.Args[1])
	if err != nil {
		log.Printf("error opening configuration file: %s", err)
		os.Exit(20)
	}

	config := &pentagon.Config{}
	err = yaml.Unmarshal(configFile, config)
	if err != nil {
		log.Printf("error parsing configuration file: %s", err)
		os.Exit(21)
	}

	config.SetDefaults()

	if err := config.Validate(); err != nil {
		log.Printf("configuration error: %s", err)
		os.Exit(22)
	}

	vaultClient, err := getVaultClient(config.Vault)
	if err != nil {
		log.Printf("unable to get vault client: %s", err)
		os.Exit(30)
	}

	k8sClient, err := getK8sClient()
	if err != nil {
		log.Printf("unable to get kubernetes client: %s", err)
		os.Exit(31)
	}

	reflector := pentagon.NewReflector(
		vaultClient.Logical(),
		k8sClient,
		config.Namespace,
		config.Label,
	)
	err = reflector.Reflect(config.Mappings)
	if err != nil {
		log.Printf("error reflecting vault values into kubernetes: %s", err)
		os.Exit(40)
	}
	successGauge.Set(1)

	if config.Daemon {
		log.Printf("running as a daemon. Refresh interval is %s", config.RefreshInterval.String())

		http.Handle("/metrics", promhttp.Handler())
		go http.ListenAndServe(config.ListenAddress, nil)
		ticker := time.NewTicker(config.RefreshInterval)
		for range ticker.C {
			err := setVaultToken(vaultClient, config.Vault)
			if err != nil {
				log.Printf("error setting vault token. %s", err)
				successGauge.Set(0)
				continue
			}
			err = reflector.Reflect(config.Mappings)
			if err != nil {
				successGauge.Set(0)
				log.Printf("error reflecting vault values into kubernetes: %s", err)
				continue
			}
			successGauge.Set(1)
		}
	}
}

func getK8sClient() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return clientset, nil
}

func getVaultClient(vaultConfig pentagon.VaultConfig) (*api.Client, error) {
	c := api.DefaultConfig()
	c.Address = vaultConfig.URL

	// Set any TLS-specific options for vault if they were provided in the
	// configuration.  The zero-value of the TLSConfig struct should be safe
	// to use anyway.
	if vaultConfig.TLSConfig != nil {
		c.ConfigureTLS(vaultConfig.TLSConfig)
	}

	client, err := api.NewClient(c)
	if err != nil {
		return nil, err
	}
	err = setVaultToken(client, vaultConfig)
	if err != nil {
		return nil, err
	}

	return client, nil
}

func setVaultToken(client *api.Client, vaultConfig pentagon.VaultConfig) error {
	switch vaultConfig.AuthType {
	case vault.AuthTypeToken:
		client.SetToken(vaultConfig.Token)
	case vault.AuthTypeGCPDefault:
		err := setVaultTokenViaGCP(client, vaultConfig.Role)
		if err != nil {
			return fmt.Errorf("unable to set token via gcp: %s", err)
		}
	case vault.AuthTypeKubernetes:
		err := setVaultTokenViaKubernetes(client, vaultConfig.Role, vaultConfig.AuthPath)
		if err != nil {
			return fmt.Errorf("unable to set token via kubernetes: %s", err)
		}
	default:
		return fmt.Errorf(
			"unsupported vault auth type: %s",
			vaultConfig.AuthType,
		)
	}
	return nil
}

func getRoleViaGCP() (string, error) {
	emailAddress, err := metadata.Get("instance/service-accounts/default/email")
	if err != nil {
		return "", fmt.Errorf("error getting default email address: %s", err)
	}
	components := strings.Split(emailAddress, "@")
	return components[0], nil
}

func setVaultTokenViaGCP(vaultClient *api.Client, role string) error {
	// if that's not provided, get it from the default service account
	var err error
	if role == "" {
		role, err = getRoleViaGCP()
		if err != nil {
			return fmt.Errorf("error getting role from gcp: %s", err)
		}
	}
	// just make a request directly to the metadata server rather
	// than going through the APIs which don't seem to wrap this functionality
	// in a terribly convenient way.
	metadataURL := url.URL{
		Path: "instance/service-accounts/default/identity",
	}

	values := url.Values{}
	vaultAddress, err := url.Parse(vaultClient.Address())
	if err != nil {
		return fmt.Errorf("error parsing vault address: %s", err)
	}
	values.Add(
		"audience",
		fmt.Sprintf("%s/vault/%s", vaultAddress.Hostname(), role),
	)
	values.Add("format", "full")
	metadataURL.RawQuery = values.Encode()

	// `jwt` should be a base64-encoded jwt.
	jwt, err := metadata.Get(metadataURL.String())
	if err != nil {
		return fmt.Errorf("error retrieving JWT from metadata API: %s", err)
	}

	vaultResp, err := vaultClient.Logical().Write(
		"auth/gcp/login",
		map[string]interface{}{
			"role": role,
			"jwt":  jwt,
		},
	)

	if err != nil {
		return fmt.Errorf("error authenticating to vault via gcp: %s", err)
	}

	vaultClient.SetToken(vaultResp.Auth.ClientToken)

	return nil
}

func setVaultTokenViaKubernetes(vaultClient *api.Client, role, authPath string) error {
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("error getting ServiceAccount token: %s", err)
	}
	if authPath == "" {
		authPath = "auth/kubernetes"
	}
	if role == "" {
		payload, err := NewServiceAccountToken(config.BearerToken)
		if err != nil {
			return fmt.Errorf("error getting role from ServiceAccount token: %s", err)
		}
		role = payload.Data["kubernetes.io/serviceaccount/service-account.name"]
	}
	vaultResp, err := vaultClient.Logical().Write(
		fmt.Sprintf("%s/login", authPath),
		map[string]interface{}{
			"role": role,
			"jwt":  config.BearerToken,
		},
	)

	if err != nil {
		return fmt.Errorf("error authenticating to vault via kubernetes: %s", err)
	}

	vaultClient.SetToken(vaultResp.Auth.ClientToken)

	return nil
}

type TokenPayload struct {
	Data map[string]string
}

func (e *TokenPayload) UnmarshalJSON(b []byte) error {
	// base64 decode the payload
	raw, err := base64.RawStdEncoding.DecodeString(string(b))
	if err != nil {
		return err
	}
	// unmarshal the raw text into our map[string]string
	return json.Unmarshal(raw, &e.Data)
}

func NewServiceAccountToken(token string) (TokenPayload, error) {
	payload := TokenPayload{}
	tokenParts := strings.Split(token, ".")
	if len(tokenParts) != 3 {
		return payload, fmt.Errorf("invalid token format")
	}
	err := json.Unmarshal([]byte(tokenParts[1]), &payload)
	return payload, err
}
