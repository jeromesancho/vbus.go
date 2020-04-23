package vBus

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
)

const (
	envHome       = "HOME"
	envVbusPath   = "VBUS_PATH"
	envVbusUrl    = "VBUS_URL"
	anonymousUser = "anonymous"
	defaultCost   = 11
)

type ExtendedNatsClient struct {
	hostname       string            // client hostname
	remoteHostname string            // remote client server hostname
	id             string            // app identifier
	env            map[string]string // environment variables
	rootFolder     string            // config folder root
	client         *nats.Conn        // client handle
}

// A Nats callback, that take data and path segment
type NatsCallback = func(data interface{}, segments []string) interface{}

// ExtendedNatsClient options.
type natsConnectOptions struct {
	HubId    string
	Login    string
	Password string
}

// Check if option contains user information.
func (o natsConnectOptions) hasUser() bool {
	return o.Login != "" && o.Password != ""
}

// Option is a function on the options for a connection.
type natsConnectOption func(*natsConnectOptions)

// Add the hub id option.
func HubId(hubId string) natsConnectOption {
	return func(o *natsConnectOptions) {
		o.HubId = hubId
	}
}

// Connect with specified user.
func WithUser(login, pwd string) natsConnectOption {
	return func(o *natsConnectOptions) {
		o.Login = login
		o.Password = pwd
	}
}

// Constructor when the server and the client are running on the same system (same hostname).
func NewExtendedNatsClient(appDomain, appId string) *ExtendedNatsClient {
	hostname := getHostname()

	client := &ExtendedNatsClient{
		hostname:       hostname,
		remoteHostname: hostname,
		id:             fmt.Sprintf("%s.%s", appDomain, appId),
		env:            readEnvVar(),
		client:         nil,
	}

	client.rootFolder = client.env[envVbusPath]

	// generate a default location is not specified
	if client.rootFolder == "" {
		client.rootFolder = path.Join(client.env[envHome], "vbus")
	}

	return client
}

func readEnvVar() map[string]string {
	return map[string]string{
		envHome:     os.Getenv(envHome),
		envVbusPath: os.Getenv(envVbusPath),
		envVbusUrl:  os.Getenv(envVbusUrl),
	}
}

// Get resolved hostname.
func (c *ExtendedNatsClient) GetHostname() string {
	return c.hostname
}

// Get application id.
func (c *ExtendedNatsClient) GetId() string {
	return c.id
}

// Try to connect.
// Available options: vBus.HubId(), vBus.WithUser()
func (c *ExtendedNatsClient) Connect(options ...natsConnectOption) error {
	// retrieve options
	opts := natsConnectOptions{}
	for _, opt := range options {
		opt(&opts)
	}

	if opts.HubId != "" {
		c.remoteHostname = opts.HubId
	} else {
		c.remoteHostname = c.hostname
	}

	if opts.hasUser() {
		url, newHost, err := c.findVbusUrl(&configuration{})
		if err != nil {
			return errors.Wrap(err, "cannot find vbus url")
		}

		// check if we need to update remote host
		if newHost != "" {
			c.remoteHostname = newHost
		}

		// connect with provided user info
		c.client, err = nats.Connect(url,
			nats.UserInfo(opts.Login, opts.Password),
			nats.Name(opts.Login))
		return err
	} else {
		config, err := c.readOrGetDefaultConfig()
		if err != nil {
			return errors.Wrap(err, "cannot retrieve configuration")
		}

		url, newHost, err := c.findVbusUrl(config)
		if err != nil {
			return errors.Wrap(err, "cannot find vbus url")
		}

		// update the config file with the new url
		config.Vbus.Url = url

		// check if we need to update remote host
		if newHost != "" {
			c.remoteHostname = newHost
		}

		err = c.saveConfigFile(config)
		if err != nil {
			return errors.Wrap(err, "cannot save configuration")
		}

		err = c.publishUser(url, config.Client)
		if err != nil {
			return errors.Wrap(err, "cannot create user")
		}
		time.Sleep(2000 * time.Millisecond)

		// connect with newly created user
		c.client, err = nats.Connect(url,
			nats.UserInfo(config.Client.User, config.Key.Private),
			nats.Name(config.Client.User))

		return err
	}
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
// Advanced Nats Functions
////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

// Advanced Nats methods options
type advOptions struct {
	Timeout  time.Duration
	WithId   bool
	WithHost bool
}

// Option is a function on the options for a connection.
type AdvOption func(*advOptions)

// Set optional timeout
func Timeout(t time.Duration) AdvOption {
	return func(o *advOptions) {
		o.Timeout = t
	}
}

// Do not include this service id before the provided path
func WithoutId() AdvOption {
	return func(o *advOptions) {
		o.WithId = false
	}
}

// Do not include this service host before the provided path
func WithoutHost() AdvOption {
	return func(o *advOptions) {
		o.WithHost = false
	}
}

// Retrieve all options to a struct
func getAdvOptions(advOpts ...AdvOption) advOptions {
	// set default options
	opts := advOptions{
		Timeout:  1000 * time.Millisecond,
		WithHost: true,
		WithId:   true,
	}
	for _, opt := range advOpts {
		opt(&opts)
	}
	return opts
}

// Compute the path with some options
func (c *ExtendedNatsClient) getPath(base string, opts advOptions) (path string) {
	path = base
	if opts.WithHost {
		path = joinPath(c.hostname, base)
	}
	if opts.WithId {
		path = joinPath(c.id, path)
	}
	return
}

func (c *ExtendedNatsClient) Request(base string, data interface{}, advOpts ...AdvOption) (interface{}, error) {
	opts := getAdvOptions(advOpts...)
	natsPath := c.getPath(base, opts)
	msg, err := c.client.Request(natsPath, toVbus(data), opts.Timeout)
	if err != nil {
		return nil, errors.Wrap(err, "cannot send request")
	}
	return fromVbus(msg.Data)
}

func (c *ExtendedNatsClient) Publish(base string, data interface{}, advOpts ...AdvOption) error {
	opts := getAdvOptions(advOpts...)
	natsPath := c.getPath(base, opts)
	return c.client.Publish(natsPath, toVbus(data))
}

// Utility method that automatically parse subject wildcard and chevron to arguments.
// If a value is returned, it is published on the reply subject.
func (c *ExtendedNatsClient) Subscribe(base string, cb NatsCallback, advOpts ...AdvOption) (*nats.Subscription, error) {
	opts := getAdvOptions(advOpts...)
	natsPath := c.getPath(base, opts)
	// create a regex that capture wildcard and chevron in path
	regex := strings.Replace(natsPath, ".", `\.`, -1)  // escape dot
	regex = strings.Replace(regex, "*", `([^.]+)`, -1) // capture wildcard
	regex = strings.Replace(regex, ">", `(.+)`, -1)    // capture chevron
	r := regexp.MustCompile(regex)

	return c.client.Subscribe(natsPath, func(msg *nats.Msg) {
		m := r.FindStringSubmatch(msg.Subject)
		// Parse data
		data, err := fromVbus(msg.Data)
		if err != nil {
			logrus.Warnf("error while calling subscribe callback: %v", err.Error())
			return
		}

		res, err := invokeFunc(cb, data, m[1:])
		if err != nil {
			log.Warnf("cannot call user callback: %v", err.Error())
			return
		}

		// if there is a reply subject, use it to send response
		if isStrNotEmpty(msg.Reply) {
			err = c.client.Publish(msg.Reply, toVbus(res))
			if err != nil {
				log.Warnf("error while sending response: %v", err.Error())
				return
			}
		}
	})
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
// Permissions
////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

// Ask for the specified permission.
func (c *ExtendedNatsClient) AskPermission(permission string) (bool, error) {
	if permission == "" {
		return false, errors.New("permission path empty")
	}

	config, err := c.readOrGetDefaultConfig()
	if err != nil {
		return false, errors.Wrap(err, "cannot read config")
	}

	config.Client.Permissions.Subscribe = append(config.Client.Permissions.Subscribe, permission)
	config.Client.Permissions.Publish = append(config.Client.Permissions.Publish, permission)
	natsPath := fmt.Sprintf("system.authorization.%s.%s.%s.permissions.set", c.remoteHostname, c.id, c.hostname)
	resp, err := c.Request(natsPath, config.Client.Permissions, Timeout(10*time.Second), WithoutId(), WithoutHost())
	if err != nil {
		return false, err
	}

	err = c.saveConfigFile(config)
	if err != nil {
		return false, errors.Wrap(err, "cannot save config")
	}

	return resp.(bool), nil
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
// Authentication
////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

// Publish on Vbus the user described in configuration.
func (c *ExtendedNatsClient) publishUser(url string, config ClientConfig) error {
	conn, err := nats.Connect(url, nats.UserInfo(anonymousUser, anonymousUser))
	if err != nil {
		return errors.Wrap(err, "cannot connect to client server")
	}
	defer conn.Close()

	data := toVbus(config)
	err = conn.Publish(fmt.Sprintf("system.authorization.%s.add", c.remoteHostname), data)
	if err != nil {
		return errors.Wrap(err, "error while publishing")
	}

	return nil
}

// Create a new user on vbus.
// Can be user with vBus.HubId() option.
func (c *ExtendedNatsClient) CreateUser(userConfig ClientConfig, options ...natsConnectOption) error {
	// retrieve options
	opts := natsConnectOptions{}
	for _, opt := range options {
		opt(&opts)
	}

	if opts.HubId != "" {
		c.remoteHostname = opts.HubId
	} else {
		c.remoteHostname = c.hostname
	}

	url, _, err := c.findVbusUrl(&configuration{}) // empty configuration
	if err != nil {
		return errors.Wrap(err, "cannot find vbus url")
	}

	return c.publishUser(url, userConfig)
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
// Find server url strategies
////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
// find Vbus server - strategy 0: get from argument
func (c *ExtendedNatsClient) getFromHubId(config *configuration) (url string, newHost string, e error) {
	if ret := net.ParseIP(c.remoteHostname); ret != nil {
		// already an ip address
		return fmt.Sprintf("nats://%s:21400", c.remoteHostname), "", nil
	} else {
		addr, err := net.LookupIP(c.remoteHostname) // resolve hostname
		if err != nil {
			return "", "", errors.Wrap(err, "Cannot resolve hostname")
		}
		return fmt.Sprintf("nats://%v:21400", addr[0]), "", nil
	}
}

// find Vbus server - strategy 1: get url from config file
func (c *ExtendedNatsClient) getFromConfigFile(config *configuration) (url string, newHost string, e error) {
	return config.Vbus.Url, "", nil
}

// find vbus server  - strategy 2: get url from ENV:VBUS_URL
func (c *ExtendedNatsClient) getFromEnv(config *configuration) (url string, newHost string, e error) {
	return c.env[envVbusUrl], "", nil
}

// find vbus server  - strategy 3: try default url client://hostname:21400
func (c *ExtendedNatsClient) getDefault(config *configuration) (url string, newHost string, e error) {
	return fmt.Sprintf("nats://%s.veeamesh.local:21400", c.hostname), "", nil
}

// find vbus server  - strategy 4: find it using avahi
func (c *ExtendedNatsClient) getFromZeroconf(config *configuration) (url string, newHost string, e error) {
	return zeroconfSearch()
}

func (c *ExtendedNatsClient) findVbusUrl(config *configuration) (serverUrl string, newHost string, e error) {
	findServerUrlStrategies := []func(config *configuration) (url string, newHost string, e error){
		c.getFromHubId,
		c.getFromConfigFile,
		c.getFromEnv,
		c.getDefault,
		c.getFromZeroconf,
	}

	success := false
	for _, strategy := range findServerUrlStrategies {
		serverUrl, newHost, e = strategy(config)
		if testVbusUrl(serverUrl) {
			log.Debugf("url found using strategy '%s': %s", getFunctionName(strategy), serverUrl)
			success = true
			break
		} else {
			log.Debugf("cannot find a valid url using strategy '%s': %s", getFunctionName(strategy), serverUrl)
		}
	}

	if !success {
		return "", "", errors.New("cannot find a valid Vbus url")
	}

	return
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
// Configuration
////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

// Permission configuration.
type PermConfig struct {
	Subscribe []string `json:"subscribe"`
	Publish   []string `json:"publish"`
}

// Hold user information
type ClientConfig struct {
	User        string     `json:"user"`
	Password    string     `json:"password"`
	Permissions PermConfig `json:"permissions"`
}

type keyConfig struct {
	Private string `json:"private"`
}

type vbusConfig struct {
	Url string `json:"url"`
}

type configuration struct {
	Client ClientConfig `json:"client"`
	Key    keyConfig    `json:"key"`
	Vbus   vbusConfig   `json:"vbus"`
}

// Try to read config file.
// If not found, it returns the default configuration.
func (c *ExtendedNatsClient) readOrGetDefaultConfig() (*configuration, error) {
	if _, err := os.Stat(c.rootFolder); os.IsNotExist(err) {
		err = os.Mkdir(c.rootFolder, os.ModeDir)
		if err != nil {
			return nil, err
		}
	}

	log.Debug("check if we already have a Vbus config file in %s" + c.rootFolder)
	configFile := path.Join(c.rootFolder, c.id) + ".conf"
	if fileExists(configFile) {
		log.Debugf("load existing configuration file for %s", c.id)
		jsonFile, err := os.Open(configFile)
		if err != nil {
			return nil, errors.Wrap(err, "cannot open config file")
		}

		bytes, err := ioutil.ReadAll(jsonFile)
		if err != nil {
			return nil, errors.Wrap(err, "cannot read config file")
		}

		var config configuration
		err = json.Unmarshal(bytes, &config)
		if err != nil {
			return nil, errors.Wrap(err, "cannot parse config file")
		}
		return &config, nil
	} else {
		log.Debugf("create new configuration file for %s", c.id)
		return c.getDefaultConfig()
	}
}

// Creates a default configuration object.
func (c *ExtendedNatsClient) getDefaultConfig() (*configuration, error) {
	log.Debugf("create new configuration file for %s", c.id)
	password, err := generatePassword()
	if err != nil {
		return nil, errors.Wrap(err, "cannot generate password")
	}

	publicKey, err := bcrypt.GenerateFromPassword([]byte(password), defaultCost)
	if err != nil {
		return nil, errors.Wrap(err, "cannot generate public key")
	}

	return &configuration{
		Client: ClientConfig{
			User:     fmt.Sprintf("%s.%s", c.id, c.hostname),
			Password: string(publicKey),
			Permissions: PermConfig{
				Subscribe: []string{
					c.id,
					fmt.Sprintf("%s.>", c.id),
				},
				Publish: []string{
					c.id,
					fmt.Sprintf("%s.>", c.id),
				},
			},
		},
		Key: keyConfig{
			Private: password,
		},
	}, nil
}

// Write configuration on disk
func (c *ExtendedNatsClient) saveConfigFile(config *configuration) error {
	data := toVbus(config)
	filepath := path.Join(c.rootFolder, c.id+".conf")
	return ioutil.WriteFile(filepath, data, 0666)
}
