// +build linux

package libcontainer

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/docker/libcontainer/cgroups"
	"github.com/docker/libcontainer/configs"
	"github.com/docker/libcontainer/netlink"
	"github.com/docker/libcontainer/seccomp"
	"github.com/docker/libcontainer/system"
	"github.com/docker/libcontainer/user"
	"github.com/docker/libcontainer/utils"
)

type initType string

const (
	initSetns    initType = "setns"
	initStandard initType = "standard"
)

type pid struct {
	Pid int `json:"pid"`
}

// network is an internal struct used to setup container networks.
type network struct {
	configs.Network

	// TempVethPeerName is a unique tempory veth peer name that was placed into
	// the container's namespace.
	TempVethPeerName string `json:"temp_veth_peer_name"`
}

// initConfig is used for transferring parameters from Exec() to Init()
type initConfig struct {
	Args             []string        `json:"args"`
	Env              []string        `json:"env"`
	Cwd              string          `json:"cwd"`
	Capabilities     []string        `json:"capabilities"`
	User             string          `json:"user"`
	Config           *configs.Config `json:"config"`
	Console          string          `json:"console"`
	Networks         []*network      `json:"network"`
	PassedFilesCount int             `json:"passed_files_count"`
}

type initer interface {
	Init() error
}

func newContainerInit(t initType, pipe *os.File) (initer, error) {
	var config *initConfig
	if err := json.NewDecoder(pipe).Decode(&config); err != nil {
		return nil, err
	}
	if err := populateProcessEnvironment(config.Env); err != nil {
		return nil, err
	}
	switch t {
	case initSetns:
		return &linuxSetnsInit{
			config: config,
		}, nil
	case initStandard:
		return &linuxStandardInit{
			parentPid: syscall.Getppid(),
			config:    config,
		}, nil
	}
	return nil, fmt.Errorf("unknown init type %q", t)
}

// populateProcessEnvironment loads the provided environment variables into the
// current processes's environment.
func populateProcessEnvironment(env []string) error {
	for _, pair := range env {
		p := strings.SplitN(pair, "=", 2)
		if len(p) < 2 {
			return fmt.Errorf("invalid environment '%v'", pair)
		}
		if err := os.Setenv(p[0], p[1]); err != nil {
			return err
		}
	}
	return nil
}

// finalizeNamespace drops the caps, sets the correct user
// and working dir, and closes any leaked file descriptors
// before executing the command inside the namespace
func finalizeNamespace(config *initConfig) error {
	// Ensure that all unwanted fds we may have accidentally
	// inherited are marked close-on-exec so they stay out of the
	// container
	if err := utils.CloseExecFrom(config.PassedFilesCount + 3); err != nil {
		return err
	}

	capabilities := config.Config.Capabilities
	if config.Capabilities != nil {
		capabilities = config.Capabilities
	}
	w, err := newCapWhitelist(capabilities)
	if err != nil {
		return err
	}

	// inject /etc/passwd until we have sufficient privs to do that
	if err := injectUserPasswd(config); err != nil {
		return err
	}

	// drop capabilities in bounding set before changing user
	if err := w.dropBoundingSet(); err != nil {
		return err
	}
	// preserve existing capabilities while we change users
	if err := system.SetKeepCaps(); err != nil {
		return err
	}
	if err := setupUser(config); err != nil {
		return err
	}
	if err := system.ClearKeepCaps(); err != nil {
		return err
	}
	// drop all other capabilities
	if err := w.drop(); err != nil {
		return err
	}
	if config.Cwd != "" {
		if err := syscall.Chdir(config.Cwd); err != nil {
			return err
		}
	}
	return nil
}

func injectUserPasswd(config *initConfig) error {
	if config.User == "" {
		return nil
	}

	var (
		suid, sgid string
		uid        int
	)

	parts := strings.Split(config.User, ":")
	switch len(parts) {
	/* UID alone */
	case 1:
		if numid, err := strconv.Atoi(config.User); err != nil {
			return nil
		} else {
			uid = numid
		}
		suid = config.User
		sgid = suid
		break
	case 2:
		if _, err := strconv.Atoi(parts[0]); err != nil {
			return nil
		}
		suid = parts[0]
		sgid = suid

		if _, err := strconv.Atoi(parts[1]); err == nil {
			sgid = parts[1]
		}

	default:
		return nil
	}

	if pf, err := user.GetPasswd(); err == nil {
		defer pf.Close()

		found, err := user.ParsePasswdFilter(pf, func(u user.User) bool {
			return u.Name == "DockerUser" || u.Uid == uid
		})

		if err != nil {
			return err
		}

		if len(found) == 0 {
			/* No DockerUser in /etc/passwd - define one */
			if fp, err := os.OpenFile("/etc/passwd", os.O_WRONLY|os.O_APPEND, os.FileMode(0666)); err == nil {
				fp.WriteString(fmt.Sprintf("%s:x:%s:%s:Docker Mapped User:/:/sbin/nologin\n", "DockerUser", suid, sgid))
				fp.Close()
			}
		}
	}

	return nil
}

// joinExistingNamespaces gets all the namespace paths specified for the container and
// does a setns on the namespace fd so that the current process joins the namespace.
func joinExistingNamespaces(namespaces []configs.Namespace) error {
	for _, ns := range namespaces {
		if ns.Path != "" {
			f, err := os.OpenFile(ns.Path, os.O_RDONLY, 0)
			if err != nil {
				return err
			}
			err = system.Setns(f.Fd(), uintptr(ns.Syscall()))
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// setupUser changes the groups, gid, and uid for the user inside the container
func setupUser(config *initConfig) error {
	// Set up defaults.
	defaultExecUser := user.ExecUser{
		Uid:  syscall.Getuid(),
		Gid:  syscall.Getgid(),
		Home: "/",
	}
	passwdPath, err := user.GetPasswdPath()
	if err != nil {
		return err
	}
	groupPath, err := user.GetGroupPath()
	if err != nil {
		return err
	}
	execUser, err := user.GetExecUserPath(config.User, &defaultExecUser, passwdPath, groupPath)
	if err != nil {
		return err
	}

	var addGroups []int
	if len(config.Config.AdditionalGroups) > 0 {
		addGroups, err = user.GetAdditionalGroupsPath(config.Config.AdditionalGroups, groupPath)
		if err != nil {
			return err
		}
	}

	suppGroups := append(execUser.Sgids, addGroups...)
	if err := syscall.Setgroups(suppGroups); err != nil {
		return err
	}

	if err := system.Setgid(execUser.Gid); err != nil {
		return err
	}
	if err := system.Setuid(execUser.Uid); err != nil {
		return err
	}
	// if we didn't get HOME already, set it based on the user's HOME
	if envHome := os.Getenv("HOME"); envHome == "" {
		if err := os.Setenv("HOME", execUser.Home); err != nil {
			return err
		}
	}

	return nil
}

// setupNetwork sets up and initializes any network interface inside the container.
func setupNetwork(config *initConfig) error {
	for _, config := range config.Networks {
		strategy, err := getStrategy(config.Type)
		if err != nil {
			return err
		}
		if err := strategy.initialize(config); err != nil {
			return err
		}
	}
	return nil
}

func setupRoute(config *configs.Config) error {
	for _, config := range config.Routes {
		if err := netlink.AddRoute(config.Destination, config.Source, config.Gateway, config.InterfaceName); err != nil {
			return err
		}
	}
	return nil
}

func setupRlimits(config *configs.Config) error {
	for _, rlimit := range config.Rlimits {
		l := &syscall.Rlimit{Max: rlimit.Hard, Cur: rlimit.Soft}
		if err := syscall.Setrlimit(rlimit.Type, l); err != nil {
			return fmt.Errorf("error setting rlimit type %v: %v", rlimit.Type, err)
		}
	}
	return nil
}

// killCgroupProcesses freezes then iterates over all the processes inside the
// manager's cgroups sending a SIGKILL to each process then waiting for them to
// exit.
func killCgroupProcesses(m cgroups.Manager) error {
	var procs []*os.Process
	if err := m.Freeze(configs.Frozen); err != nil {
		logrus.Warn(err)
	}
	pids, err := m.GetPids()
	if err != nil {
		m.Freeze(configs.Thawed)
		return err
	}
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			procs = append(procs, p)
			if err := p.Kill(); err != nil {
				logrus.Warn(err)
			}
		}
	}
	if err := m.Freeze(configs.Thawed); err != nil {
		logrus.Warn(err)
	}
	for _, p := range procs {
		if _, err := p.Wait(); err != nil {
			logrus.Warn(err)
		}
	}
	return nil
}

func finalizeSeccomp(config *initConfig) error {
	if config.Config.Seccomp == nil {
		return nil
	}
	context := seccomp.New()
	for _, s := range config.Config.Seccomp.Syscalls {
		ss := &seccomp.Syscall{
			Value:  uint32(s.Value),
			Action: seccompAction(s.Action),
		}
		if len(s.Args) > 0 {
			ss.Args = seccompArgs(s.Args)
		}
		context.Add(ss)
	}
	return context.Load()
}

func seccompAction(a configs.Action) seccomp.Action {
	switch a {
	case configs.Kill:
		return seccomp.Kill
	case configs.Trap:
		return seccomp.Trap
	case configs.Allow:
		return seccomp.Allow
	}
	return seccomp.Error(syscall.Errno(int(a)))
}

func seccompArgs(args []*configs.Arg) seccomp.Args {
	var sa []seccomp.Arg
	for _, a := range args {
		sa = append(sa, seccomp.Arg{
			Index: uint32(a.Index),
			Op:    seccompOperator(a.Op),
			Value: uint(a.Value),
		})
	}
	return seccomp.Args{sa}
}

func seccompOperator(o configs.Operator) seccomp.Operator {
	switch o {
	case configs.EqualTo:
		return seccomp.EqualTo
	case configs.NotEqualTo:
		return seccomp.NotEqualTo
	case configs.GreatherThan:
		return seccomp.GreatherThan
	case configs.LessThan:
		return seccomp.LessThan
	case configs.MaskEqualTo:
		return seccomp.MaskEqualTo
	}
	return 0
}
