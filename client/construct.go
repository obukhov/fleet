// Copyright 2014 The fleet Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	etcd "github.com/coreos/etcd/client"

	"github.com/coreos/fleet/log"
	"github.com/coreos/fleet/pkg"
	"github.com/coreos/fleet/registry"
	"github.com/coreos/fleet/ssh"
	"github.com/coreos/fleet/version"
)

const (
	ClientDriverAPI  = "API"
	ClientDriverEtcd = "etcd"

	oldVersionWarning = `####################################################################
WARNING: fleetctl (%s) is older than the latest registered
version of fleet found in the cluster (%s). You are strongly
recommended to upgrade fleetctl to prevent incompatibility issues.
####################################################################
`

	DefaultEndpoint = "unix:///var/run/fleet.sock"
)

type SSHConfig struct {
	Tunnel                string
	SSHUserName           string
	StrictHostKeyChecking bool
	KnownHostsFile        string
	SshTimeout            float64
}

type ClientConfig struct {
	*SSHConfig

	ClientDriver string
	EndPoint     string
	ReqTimeout   float64

	CAFile   string
	CertFile string
	KeyFile  string

	EtcdKeyPrefix   string
	ExperimentalAPI bool
}

// getClient initializes a client of fleet based on CLI flags
func GetClient(cfg *ClientConfig) (API, error) {
	switch cfg.ClientDriver {
	case ClientDriverAPI:
		return getHTTPClient(cfg)
	case ClientDriverEtcd:
		return getRegistryClient(cfg)
	}

	return nil, fmt.Errorf("unrecognized driver %q", cfg.ClientDriver)
}

func getHTTPClient(cfg *ClientConfig) (API, error) {
	endpoints := strings.Split(cfg.EndPoint, ",")
	if len(endpoints) > 1 {
		log.Warningf("multiple endpoints provided but only the first (%s) is used", endpoints[0])
	}

	ep, err := url.Parse(endpoints[0])
	if err != nil {
		return nil, err
	}

	if len(ep.Scheme) == 0 {
		return nil, errors.New("URL scheme undefined")
	}

	tun := GetTunnelFlag(cfg.SSHConfig)
	tunneling := tun != ""

	dialUnix := ep.Scheme == "unix" || ep.Scheme == "file"

	tunnelFunc := net.Dial
	if tunneling {
		sshClient, err := ssh.NewSSHClient(cfg.SSHUserName, tun, GetChecker(cfg.SSHConfig), true, GetSSHTimeoutFlag(cfg.SSHConfig))
		if err != nil {
			return nil, fmt.Errorf("failed initializing SSH client: %v", err)
		}

		if dialUnix {
			tgt := ep.Path
			tunnelFunc = func(string, string) (net.Conn, error) {
				log.Debugf("Establishing remote fleetctl proxy to %s", tgt)
				cmd := fmt.Sprintf(`fleetctl fd-forward %s`, tgt)
				return ssh.DialCommand(sshClient, cmd)
			}
		} else {
			tunnelFunc = sshClient.Dial
		}
	}

	dialFunc := tunnelFunc
	if dialUnix {
		// This commonly happens if the user misses the leading slash after the scheme.
		// For example, "unix://var/run/fleet.sock" would be parsed as host "var".
		if len(ep.Host) > 0 {
			return nil, fmt.Errorf("unable to connect to host %q with scheme %q", ep.Host, ep.Scheme)
		}

		// The Path field is only used for dialing and should not be used when
		// building any further HTTP requests.
		sockPath := ep.Path
		ep.Path = ""

		// If not tunneling to the unix socket, http.Client will dial it directly.
		// http.Client does not natively support dialing a unix domain socket, so the
		// dial function must be overridden.
		if !tunneling {
			dialFunc = func(string, string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			}
		}

		// http.Client doesn't support the schemes "unix" or "file", but it
		// is safe to use "http" as dialFunc ignores it anyway.
		ep.Scheme = "http"

		// The Host field is not used for dialing, but will be exposed in debug logs.
		ep.Host = "domain-sock"
	}

	tlsConfig, err := pkg.ReadTLSConfigFiles(cfg.CAFile, cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, err
	}

	trans := pkg.LoggingHTTPTransport{
		Transport: http.Transport{
			Dial:            dialFunc,
			TLSClientConfig: tlsConfig,
		},
	}

	hc := http.Client{
		Transport: &trans,
	}

	return NewHTTPClient(&hc, *ep)
}

func getEndpoint(cfg *ClientConfig) string {
	// The user explicitly set --experimental-api=false, so it trumps the
	// --driver flag. This behavior exists for backwards-compatibilty.
	if !cfg.ExperimentalAPI {
		// Additionally, if the user set --experimental-api=false and did
		// not change the value of --endpoint, they likely want to use the
		// old default value.
		if cfg.EndPoint == DefaultEndpoint {
			return "http://127.0.0.1:2379,http://127.0.0.1:4001"
		}
	}
	return cfg.EndPoint
}

func getRegistryClient(cfg *ClientConfig) (API, error) {
	var dial func(string, string) (net.Conn, error)
	tun := GetTunnelFlag(cfg.SSHConfig)
	if tun != "" {
		sshClient, err := ssh.NewSSHClient(cfg.SSHUserName, tun, GetChecker(cfg.SSHConfig), false, GetSSHTimeoutFlag(cfg.SSHConfig))
		if err != nil {
			return nil, fmt.Errorf("failed initializing SSH client: %v", err)
		}

		dial = func(network, addr string) (net.Conn, error) {
			tcpaddr, err := net.ResolveTCPAddr(network, addr)
			if err != nil {
				return nil, err
			}
			return sshClient.DialTCP(network, nil, tcpaddr)
		}
	}

	tlsConfig, err := pkg.ReadTLSConfigFiles(cfg.CAFile, cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, err
	}

	trans := &http.Transport{
		Dial:            dial,
		TLSClientConfig: tlsConfig,
	}

	eCfg := etcd.Config{
		Endpoints:               strings.Split(getEndpoint(cfg), ","),
		Transport:               trans,
		HeaderTimeoutPerRequest: getRequestTimeoutFlag(cfg),
	}

	eClient, err := etcd.New(eCfg)
	if err != nil {
		return nil, err
	}

	kAPI := etcd.NewKeysAPI(eClient)
	reg := registry.NewEtcdRegistry(kAPI, cfg.EtcdKeyPrefix)

	if msg, ok := checkVersion(reg); !ok {
		return &RegistryClient{Registry: reg}, errors.New(msg)
	}

	return &RegistryClient{Registry: reg}, nil
}

// checkVersion makes a best-effort attempt to verify that fleetctl is at least as new as the
// latest fleet version found registered in the cluster. If any errors are encountered or fleetctl
// is >= the latest version found, it returns true. If it is < the latest found version, it returns
// false and a scary warning to the user.
func checkVersion(cReg registry.ClusterRegistry) (string, bool) {
	fv := version.SemVersion
	lv, err := cReg.LatestDaemonVersion()
	if err != nil {
		log.Errorf("error attempting to check latest fleet version in Registry: %v", err)
	} else if lv != nil && fv.LessThan(*lv) {
		return fmt.Sprintf(oldVersionWarning, fv.String(), lv.String()), false
	}
	return "", true
}

// GetChecker creates and returns a HostKeyChecker, or nil if any error is encountered
func GetChecker(cfg *SSHConfig) *ssh.HostKeyChecker {
	if !cfg.StrictHostKeyChecking {
		return nil
	}

	keyFile := ssh.NewHostKeyFile(cfg.KnownHostsFile)
	return ssh.NewHostKeyChecker(keyFile)
}

func GetTunnelFlag(cfg *SSHConfig) string {
	tun := cfg.Tunnel
	if tun != "" && !strings.Contains(tun, ":") {
		tun += ":22"
	}
	return tun
}

func GetSSHTimeoutFlag(cfg *SSHConfig) time.Duration {
	return time.Duration(cfg.SshTimeout*1000) * time.Millisecond
}

func getRequestTimeoutFlag(cfg *ClientConfig) time.Duration {
	return time.Duration(cfg.ReqTimeout*1000) * time.Millisecond
}
