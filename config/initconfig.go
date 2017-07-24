// Copyright 2016 The Upspin Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package config creates a client configuration from various sources.
package config // import "upspin.io/config"

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	osuser "os/user"
	"path/filepath"
	"strings"

	yaml "gopkg.in/yaml.v2"

	"upspin.io/errors"
	"upspin.io/factotum"
	"upspin.io/log"
	"upspin.io/pack"
	"upspin.io/upspin"
	"upspin.io/user"

	// Needed because the default packing is "ee" and its
	// implementation is referenced if no packing is specified.
	_ "upspin.io/pack/ee"
)

var inTest = false // Generate errors instead of logs for certain problems.

// base implements upspin.Config, returning default values for all operations.
type base struct{}

func (base) UserName() upspin.UserName      { return defaultUserName }
func (base) Factotum() upspin.Factotum      { return nil }
func (base) Packing() upspin.Packing        { return defaultPacking }
func (base) KeyEndpoint() upspin.Endpoint   { return defaultKeyEndpoint }
func (base) DirEndpoint() upspin.Endpoint   { return upspin.Endpoint{} }
func (base) StoreEndpoint() upspin.Endpoint { return upspin.Endpoint{} }
func (base) Value(string) string            { return "" }

// New returns a config with all fields set as defaults.
func New() upspin.Config {
	return base{}
}

var (
	defaultUserName    = upspin.UserName("noone@nowhere.org")
	defaultPacking     = upspin.EEPack
	defaultKeyEndpoint = upspin.Endpoint{
		Transport: upspin.Remote,
		NetAddr:   "key.upspin.io:443",
	}
)

// Known keys.
const (
	username    = "username"
	keyserver   = "keyserver"
	dirserver   = "dirserver"
	storeserver = "storeserver"
	packing     = "packing"
	secrets     = "secrets"
)

// ErrNoFactotum indicates that the returned config contains no Factotum, and
// that the user requested this by setting secrets=none in the configuration.
var ErrNoFactotum = errors.Str("factotum not initialized: no secrets provided")

// FromFile initializes a config using the given file. If the file cannot
// be opened but the name can be found in $HOME/upspin, that file is used.
func FromFile(name string) (upspin.Config, error) {
	f, err := os.Open(name)
	if err != nil && !filepath.IsAbs(name) && os.IsNotExist(err) {
		// It's a local name, so, try adding $HOME/upspin
		home, errHome := Homedir()
		if errHome == nil {
			f, err = os.Open(filepath.Join(home, "upspin", name))
		}
	}
	if err != nil {
		const op = "config.FromFile"
		if os.IsNotExist(err) {
			return nil, errors.E(op, errors.NotExist, err)
		}
		return nil, errors.E(op, err)
	}
	defer f.Close()
	return InitConfig(f)
}

// InitConfig returns a config generated by parsing the contents of
// the io.Reader, typically a configuration file.
//
// A configuration file should be of the format
//   # lines that begin with a hash are ignored
//   key = value
// where key may be one of username, keyserver, dirserver, storeserver,
// packing, secrets, or tlscerts.
//
// The default configuration file location is $HOME/upspin/config.
// If passed a non-nil io.Reader, that is used instead of the default file.
//
// Any endpoints (keyserver, dirserver, storeserver) not set in the data for
// the config will be set to the "unassigned" transport and an empty network
// address, except keyserver which defaults to "remote,key.upspin.io:443".
// If an endpoint is specified without a transport it is assumed to be
// the address component of a remote endpoint.
// If a remote endpoint is specified without a port in its address component
// the port is assumed to be 443.
//
// The default value for packing is "ee".
//
// The default value for secrets is "$HOME/.ssh".
// The special value "none" indicates there are no secrets to load;
// in this case, the returned config will not include a Factotum
// and the returned error is ErrNoFactotum.
//
// The tlscerts key specifies a directory containing PEM certificates define
// the certificate pool used for verifying client TLS connections,
// replacing the root certificate list provided by the operating system.
// Files without the suffix ".pem" are ignored.
// The default value for tlscerts is the empty string,
// in which case just the system roots are used.
func InitConfig(r io.Reader) (upspin.Config, error) {
	const op = "config.InitConfig"
	vals := map[string]string{
		username:    string(defaultUserName),
		packing:     defaultPacking.String(),
		keyserver:   defaultKeyEndpoint.String(),
		dirserver:   "",
		storeserver: "",
	}
	other := make(map[string]interface{})

	// If the provided reader is nil, try $HOME/upspin/config.
	if r == nil {
		home, err := Homedir()
		if err != nil {
			return nil, errors.E(op, err)
		}
		f, err := os.Open(filepath.Join(home, "upspin/config"))
		if err != nil {
			return nil, errors.E(op, err)
		}
		r = f
		defer f.Close()
	}

	// Read the YAML definition.
	data, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, errors.E(op, err)
	}
	if err := valsFromYAML(vals, other, data); err != nil {
		return nil, errors.E(op, err)
	}

	// Construct a config from vals.
	cfg := New()

	// Put the canonical respresentation of the username in the config.
	username, err := user.Clean(upspin.UserName(vals[username]))
	if err != nil {
		return nil, errors.E(op, err)
	}
	cfg = SetUserName(cfg, username)

	packer := pack.LookupByName(vals[packing])
	if packer == nil {
		return nil, errors.E(op, errors.Invalid, errors.Errorf("unknown packing %q", vals[packing]))
	}
	cfg = SetPacking(cfg, packer.Packing())

	dir := ""
	if dirV, ok := other[secrets]; ok {
		dir, ok = dirV.(string)
		if !ok {
			return nil, errors.E(op, errors.Errorf("invalid type for secrets: %T", dirV))
		}
	}
	if dir == "" {
		dir, err = sshdir()
		if err != nil {
			return nil, errors.E(op, errors.Errorf("cannot find .ssh directory: %v", err))
		}
	}
	if dir == "none" {
		err = ErrNoFactotum
	} else {
		f, err := factotum.NewFromDir(dir)
		if err != nil {
			return nil, errors.E(op, err)
		}
		cfg = SetFactotum(cfg, f)
		// This must be done before bind so that keys are ready for
		// authenticating to servers.
	}

	cfg = SetKeyEndpoint(cfg, parseEndpoint(op, vals, keyserver, &err))
	cfg = SetStoreEndpoint(cfg, parseEndpoint(op, vals, storeserver, &err))
	cfg = SetDirEndpoint(cfg, parseEndpoint(op, vals, dirserver, &err))

	valueMap := make(map[string]string)
	for k, v := range other {
		key, err := asString(k)
		if err != nil {
			return nil, errors.E(op, errors.Invalid, err)
		}
		b, err := yaml.Marshal(v)
		if err != nil {
			return nil, errors.E(op, errors.Invalid, errors.Errorf("bad value for config key %v: %v", key, err))
		}
		valueMap[key] = string(bytes.TrimSpace(b))
	}
	cfg = cfgValueMap{cfg, valueMap}

	return cfg, err
}

// valsFromYAML parses YAML from the given map and puts the values
// into the provided map. Unrecognized keys generate an error.
func valsFromYAML(vals map[string]string, other map[string]interface{}, data []byte) error {
	newVals := map[string]interface{}{}
	if err := yaml.Unmarshal(data, newVals); err != nil {
		return errors.E(errors.Invalid, errors.Errorf("parsing YAML file: %v", err))
	}
	for k, v := range newVals {
		if _, ok := vals[k]; ok {
			s, err := asString(v)
			if err != nil {
				return fmt.Errorf("%q: %v", k, err)
			}
			vals[k] = s
			continue
		}
		other[k] = v
	}
	return nil
}

// asString tries to convert a value back into its original string. This will
// not always be possible but should be for all our expected use cases.
func asString(v interface{}) (string, error) {
	switch vc := v.(type) {
	case int, int32, int64, uint, uint32, uint64, float32, float64, bool:
		return fmt.Sprintf("%v", vc), nil
	case string:
		return vc, nil
	}
	return "", errors.E(errors.Invalid, errors.Errorf("unrecognized value %T", v))
}

func parseEndpoint(op string, vals map[string]string, key string, errorp *error) upspin.Endpoint {
	text, ok := vals[key]
	if !ok || text == "" {
		return upspin.Endpoint{}
	}

	ep, err := upspin.ParseEndpoint(text)
	// If no transport is provided, assume remote transport.
	if err != nil && !strings.Contains(text, ",") {
		if ep2, err2 := upspin.ParseEndpoint("remote," + text); err2 == nil {
			ep = ep2
			err = nil
		}
	}
	if err != nil {
		err = errors.E(op, errors.Errorf("cannot parse service %q: %v", text, err))
		log.Error.Print(err)
		if *errorp == nil {
			*errorp = err
		}
		return upspin.Endpoint{}
	}

	// If it's a remote and the provided address does not include a port,
	// assume port 443.
	if ep.Transport == upspin.Remote && !strings.Contains(string(ep.NetAddr), ":") {
		ep.NetAddr += ":443"
	}

	return *ep
}

type cfgUserName struct {
	upspin.Config
	userName upspin.UserName
}

func (cfg cfgUserName) UserName() upspin.UserName {
	return cfg.userName
}

// SetUserName returns a config derived from the given config
// with the given user name.
func SetUserName(cfg upspin.Config, u upspin.UserName) upspin.Config {
	return cfgUserName{
		Config:   cfg,
		userName: u,
	}
}

type cfgFactotum struct {
	upspin.Config
	factotum upspin.Factotum
}

func (cfg cfgFactotum) Factotum() upspin.Factotum {
	return cfg.factotum
}

// SetFactotum returns a config derived from the given config
// with the given factotum.
func SetFactotum(cfg upspin.Config, f upspin.Factotum) upspin.Config {
	return cfgFactotum{
		Config:   cfg,
		factotum: f,
	}
}

type cfgPacking struct {
	upspin.Config
	packing upspin.Packing
}

func (cfg cfgPacking) Packing() upspin.Packing {
	return cfg.packing
}

// SetPacking returns a config derived from the given config
// with the given packing.
func SetPacking(cfg upspin.Config, p upspin.Packing) upspin.Config {
	return cfgPacking{
		Config:  cfg,
		packing: p,
	}
}

type cfgKeyEndpoint struct {
	upspin.Config
	keyEndpoint upspin.Endpoint
}

func (cfg cfgKeyEndpoint) KeyEndpoint() upspin.Endpoint {
	return cfg.keyEndpoint
}

// SetKeyEndpoint returns a config derived from the given config
// with the given key endpoint.
func SetKeyEndpoint(cfg upspin.Config, e upspin.Endpoint) upspin.Config {
	return cfgKeyEndpoint{
		Config:      cfg,
		keyEndpoint: e,
	}
}

type cfgStoreEndpoint struct {
	upspin.Config
	storeEndpoint upspin.Endpoint
}

func (cfg cfgStoreEndpoint) StoreEndpoint() upspin.Endpoint {
	return cfg.storeEndpoint
}

// SetStoreEndpoint returns a config derived from the given config
// with the given store endpoint.
func SetStoreEndpoint(cfg upspin.Config, e upspin.Endpoint) upspin.Config {
	return cfgStoreEndpoint{
		Config:        cfg,
		storeEndpoint: e,
	}
}

type cfgCacheEndpoint struct {
	upspin.Config
	cacheEndpoint upspin.Endpoint
}

func (cfg cfgCacheEndpoint) CacheEndpoint() upspin.Endpoint {
	return cfg.cacheEndpoint
}

// SetCacheEndpoint returns a config derived from the given config
// with the given cache endpoint.
func SetCacheEndpoint(cfg upspin.Config, e upspin.Endpoint) upspin.Config {
	return cfgCacheEndpoint{
		Config:        cfg,
		cacheEndpoint: e,
	}
}

type cfgDirEndpoint struct {
	upspin.Config
	dirEndpoint upspin.Endpoint
}

func (cfg cfgDirEndpoint) DirEndpoint() upspin.Endpoint {
	return cfg.dirEndpoint
}

// SetDirEndpoint returns a config derived from the given config
// with the given dir endpoint.
func SetDirEndpoint(cfg upspin.Config, e upspin.Endpoint) upspin.Config {
	return cfgDirEndpoint{
		Config:      cfg,
		dirEndpoint: e,
	}
}

type cfgValue struct {
	upspin.Config
	key, val string
}

func (cfg cfgValue) Value(key string) string {
	if key == cfg.key {
		return cfg.val
	}
	return cfg.Config.Value(key)
}

// SetValue returns a config derived from the given config that contains
// the given key/value pair.
func SetValue(cfg upspin.Config, key, val string) upspin.Config {
	return cfgValue{
		Config: cfg,
		key:    key,
		val:    val,
	}
}

type cfgValueMap struct {
	upspin.Config
	values map[string]string
}

func (cfg cfgValueMap) Value(key string) string {
	v, ok := cfg.values[key]
	if !ok {
		return cfg.Config.Value(key)
	}
	return v
}

// SetFlagValues updates any flag that is still at its default value.
// It will apply all the flags possible and return the last error seen.
func SetFlagValues(cfg upspin.Config, cmd string) error {
	const op = "config.SetFlagValues"
	flagYAML := cfg.Value("cmdflags")
	if flagYAML == "" {
		return nil
	}
	cmdflags := make(map[interface{}]interface{})
	err := yaml.Unmarshal([]byte(flagYAML), cmdflags)
	if err != nil {
		return errors.E(op, errors.Invalid, errors.Errorf("bad cmdflags value: %v", err))
	}
	flags, ok := cmdflags[cmd].(map[interface{}]interface{})
	if !ok {
		return errors.E(op, errors.Invalid, errors.Errorf("bad cmdflags for %v: %T", cmd, cmdflags[cmd]))
	}
	for k, v := range flags {
		name, err := asString(k)
		if err != nil {
			return errors.E(op, errors.Invalid, errors.Errorf("bad flag name %v: %v", k, err))
		}
		val, err := asString(v)
		if err != nil {
			return errors.E(op, errors.Invalid, errors.Errorf("bad flag value for %v: %v", name, err))
		}

		f := flag.Lookup(name)
		if f == nil {
			return errors.E(op, errors.Invalid, errors.Errorf("unknown flag %q", k))
		}
		if f.Value.String() != f.DefValue {
			continue
		}
		if err := flag.Set(name, val); err != nil {
			continue
		}
	}
	return nil
}

// Homedir returns the home directory of the OS' logged-in user.
// TODO(adg): move to osutil package?
func Homedir() (string, error) {
	u, err := osuser.Current()
	// user.Current may return an error, but we should only handle it if it
	// returns a nil user. This is because os/user is wonky without cgo,
	// but it should work well enough for our purposes.
	if u == nil {
		e := errors.Str("lookup of current user failed")
		if err != nil {
			e = errors.Errorf("%v: %v", e, err)
		}
		return "", e
	}
	h := u.HomeDir
	if h == "" {
		return "", errors.E(errors.NotExist, errors.Str("user home directory not found"))
	}
	if err := isDir(h); err != nil {
		return "", err
	}
	return h, nil
}

// Home returns the home directory of the user, or panics if it cannot find one.
func Home() string {
	home, err := Homedir()
	if err != nil {
		panic(err)
	}
	return home
}

func sshdir() (string, error) {
	h, err := Homedir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(h, ".ssh")
	if err := isDir(p); err != nil {
		return "", err
	}
	return p, nil
}

func isDir(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return errors.E(errors.IO, err)
	}
	if !fi.IsDir() {
		return errors.E(errors.NotDir, errors.Str(p))
	}
	return nil
}
