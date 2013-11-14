package monitor

import (
	"atlantis/supervisor/rpc/types"
	"bufio"
	"encoding/gob"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/jigish/go-flags"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	OK = iota
	Warning
	Critical
	Uknown
)

type Config struct {
	ContainerFile   string `toml:"container_file"`
	SSHIdentity     string `toml:"ssh_identity"`
	SSHUser         string `toml:"ssh_user"`
	CheckName       string `toml:"check_name"`
	CheckDir        string `toml:"check_dir"`
	TimeoutDuration uint   `toml:"timeout_duration"`
}

type Opts struct {
	ContainerFile   string `short:"f" long:"container-file" description:"file to get contianers information from"`
	SSHIdentity     string `short:"i" long:"ssh-identity" description:"file containing the SSH key for all containers"`
	SSHUser         string `short:"u" long:"ssh-user" description:"user account to ssh into containers"`
	CheckName       string `short:"n" long:"check-name" description:"service name that will appear in Nagios for the monitor"`
	CheckDir        string `short:"d" long:"check-dir" description:"directory containing all the scripts for the monitoring checks"`
	TimeoutDuration uint   `short:"t" long:"timeout-duration" description:"max number of seconds to wait for a monitoring check to finish"`
	Config          string `short:"c" long:"config-file" default:"/etc/atlantis/supervisor/monitor.toml" description:"the config file to use"`
}

type ServiceCheck struct {
	Service  string
	User     string
	Identity string
	Host     string
	Port     uint16
	Script   string
}

//TODO(mchandra):Need defaults defined by constants
var config = &Config{
	ContainerFile:   "/etc/atlantis/supervisor/save/containers",
	SSHIdentity:     "/opt/atlantis/supervisor/master_id_rsa",
	SSHUser:         "root",
	CheckName:       "ContainerMonitor",
	CheckDir:        "/check_mk_checks",
	TimeoutDuration: 110,
}

func (s *ServiceCheck) cmd() *exec.Cmd {
	return silentSshCmd(s.User, s.Identity, s.Host, s.Script, s.Port)
}

func (s *ServiceCheck) timeOutMsg() string {
	return fmt.Sprintf("%d %s - Timeout occured during check\n", Critical, s.Service)
}

func (s *ServiceCheck) errMsg(err error) string {
	if err != nil {
		return fmt.Sprintf("%d %s - %s\n", Critical, s.Service, err.Error())
	} else {
		return fmt.Sprintf("%d %s - Error encountered while monitoring the service\n", Critical, s.Service)
	}
}

func (s *ServiceCheck) validate(msg string) string {
	m := strings.SplitN(msg, " ", 4)
	if len(m) > 1 && m[1] == s.Service {
		return msg
	}
	return s.errMsg(nil)
}

func (s *ServiceCheck) runCheck(done chan bool) {
	out, err := s.cmd().Output()
	if err != nil {
		fmt.Print(s.errMsg(err))
	} else {
		fmt.Print(s.validate(string(out)))
	}
	done <- true
}

func (s *ServiceCheck) checkWithTimeout(results chan bool, d time.Duration) {
	done := make(chan bool, 1)
	go s.runCheck(done)
	select {
	case <-done:
		results <- true
	case <-time.After(d):
		fmt.Print(s.timeOutMsg())
		results <- true
	}
}

type ContainerCheck struct {
	Name      string
	User      string
	Identity  string
	Directory string
	container *types.Container
}

func (c *ContainerCheck) Run(t time.Duration, done chan bool) {
	defer func() { done <- true }()
	o, err := silentSshCmd(c.User, c.Identity, c.container.Host, "ls "+c.Directory, c.container.SSHPort).Output()
	if err != nil {
		fmt.Printf("%d %s - Error getting checks for container : %s\n", Critical, c.Name, err.Error())
		return
	}
	fmt.Printf("%d %s - Got checks for container\n", OK, c.Name)
	scripts := strings.Split(strings.TrimSpace(string(o)), "\n")
	if len(scripts) == 0 || len(scripts[0]) == 0 {
		// nothing to check on this container, exit
		return
	}
	c.checkAll(scripts, t)
}

func (c *ContainerCheck) checkAll(scripts []string, t time.Duration) {
	results := make(chan bool, len(scripts))
	for _, s := range scripts {
		go c.serviceCheck(s).checkWithTimeout(results, t)
	}
	for _ = range scripts {
		<-results
	}
}

func (c *ContainerCheck) serviceCheck(script string) *ServiceCheck {
	// The full path to the script is required
	command := fmt.Sprintf("%s/%s %d %s", c.Directory, script, c.container.PrimaryPort, c.container.Id)
	// The service name is obtained be removing the file extension from the script and appending the container
	// id
	serviceName := fmt.Sprintf("%s_%s", strings.Split(script, ".")[0], c.container.Id)
	return &ServiceCheck{serviceName, c.User, c.Identity, c.container.Host, c.container.SSHPort, command}
}

func silentSshCmd(user, identity, host, cmd string, port uint16) *exec.Cmd {
	args := []string{"-q", user + "@" + host, "-i", identity, "-p", fmt.Sprintf("%d", port), "-o", "StrictHostKeyChecking=no", cmd}
	return exec.Command("ssh", args...)
}

// Use gob to retrieve an object from a file
func retrieveObject(file, name string, object interface{}) bool {
	fi, err := os.Open(file)
	if err != nil {
		fmt.Printf("%d %s - Could not retrieve %s: %s\n", Critical, name, file, err)
		return false
	}
	fmt.Printf("%d %s - Able to open %s\n", OK, name, file)
	r := bufio.NewReader(fi)
	d := gob.NewDecoder(r)
	d.Decode(object)
	return true
}

func overlayConfig() {
	opts := &Opts{}
	flags.Parse(opts)
	if opts.Config != "" {
		_, err := toml.DecodeFile(opts.Config, config)
		if err != nil {
			// no need to panic here. we have reasonable defaults.
		}
	}
	if opts.ContainerFile != "" {
		config.ContainerFile = opts.ContainerFile
	}
	if opts.SSHIdentity != "" {
		config.SSHIdentity = opts.SSHIdentity
	}
	if opts.SSHUser != "" {
		config.SSHUser = opts.SSHUser
	}
	if opts.CheckDir != "" {
		config.CheckDir = opts.CheckDir
	}
	if opts.CheckName != "" {
		config.CheckName = opts.CheckName
	}
	if opts.TimeoutDuration != 0 {
		config.TimeoutDuration = opts.TimeoutDuration
	}
}

//file containing containers and service name to show in Nagios for the monitor itself
func Run() {
	overlayConfig()
	var containers map[string]*types.Container
	if !retrieveObject(config.ContainerFile, config.CheckName, &containers) {
		return
	}
	done := make(chan bool, len(containers))
	config.SSHIdentity = strings.Replace(config.SSHIdentity, "~", os.Getenv("HOME"), 1)
	for _, c := range containers {
		if c.Host == "" {
			c.Host = "localhost"
		}
		check := &ContainerCheck{config.CheckName + "_" + c.Id, config.SSHUser, config.SSHIdentity, config.CheckDir, c}
		go check.Run(time.Duration(config.TimeoutDuration)*time.Second, done)
	}
	for _ = range containers {
		<-done
	}
}