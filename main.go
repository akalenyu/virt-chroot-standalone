package main

import (
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

var (
	mntNamespace string
	cpuTime      uint64
	memoryBytes  uint64
	targetUser   string
)

func init() {
	// main needs to be locked on one thread and no go routines
	runtime.LockOSThread()
}

func main() {

	rootCmd := &cobra.Command{
		Use: "virt-chroot",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {

			if mntNamespace != "" {
				// join the mount namespace of a process
				fd, err := os.Open(mntNamespace)
				if err != nil {
					return fmt.Errorf("failed to open mount namespace: %v", err)
				}
				defer fd.Close()

				if err = unix.Unshare(unix.CLONE_NEWNS); err != nil {
					return fmt.Errorf("failed to detach from parent mount namespace: %v", err)
				}
				if err := unix.Setns(int(fd.Fd()), unix.CLONE_NEWNS); err != nil {
					return fmt.Errorf("failed to join the mount namespace: %v", err)
				}
			}

			// Looking up users needs resources, let's do it before we set rlimits.
			var u *user.User
			if targetUser != "" {
				var err error
				u, err = user.Lookup(targetUser)
				if err != nil {
					return fmt.Errorf("failed to look up user: %v", err)
				}
			}

			if cpuTime > 0 {
				value := &syscall.Rlimit{
					Cur: cpuTime,
					Max: cpuTime,
				}
				err := syscall.Setrlimit(unix.RLIMIT_CPU, value)
				if err != nil {
					return fmt.Errorf("error setting prlimit on cpu time with value %d: %v", value, err)
				}
			}

			if memoryBytes > 0 {
				value := &syscall.Rlimit{
					Cur: memoryBytes,
					Max: memoryBytes,
				}
				err := syscall.Setrlimit(unix.RLIMIT_AS, value)
				if err != nil {
					return fmt.Errorf("error setting prlimit on virtual memory with value %d: %v", value, err)
				}
			}

			// Now let's switch users and drop privileges
			if u != nil {
				uid, err := strconv.ParseInt(u.Uid, 10, 32)
				if err != nil {
					return fmt.Errorf("failed to parse uid: %v", err)
				}
				gid, err := strconv.ParseInt(u.Gid, 10, 32)
				if err != nil {
					return fmt.Errorf("failed to parse gid: %v", err)
				}
				err = unix.Setgroups([]int{int(gid)})
				if err != nil {
					return fmt.Errorf("failed to drop auxiliary groups: %v", err)
				}
				_, _, errno := syscall.Syscall(syscall.SYS_SETGID, uintptr(gid), 0, 0)
				if errno != 0 {
					return fmt.Errorf("failed to join the group of the user: %v", err)
				}
				_, _, errno = syscall.Syscall(syscall.SYS_SETUID, uintptr(uid), 0, 0)
				if errno != 0 {
					return fmt.Errorf("failed to switch to user: %v", err)
				}
			}
			return nil

		},
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Printf(cmd.UsageString())
		},
	}

	rootCmd.PersistentFlags().Uint64Var(&cpuTime, "cpu", 0, "cpu time in seconds for the process")
	rootCmd.PersistentFlags().Uint64Var(&memoryBytes, "memory", 0, "memory in bytes for the process")
	rootCmd.PersistentFlags().StringVar(&mntNamespace, "mount", "", "mount namespace to use")
	rootCmd.PersistentFlags().StringVar(&targetUser, "user", "", "switch to this targetUser to e.g. drop privileges")

	execCmd := &cobra.Command{
		Use:   "exec",
		Short: "execute a sandboxed command in a specific mount namespace",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			err := syscall.Exec(args[0], args, os.Environ())
			if err != nil {
				return fmt.Errorf("failed to execute command: %v", err)
			}
			return nil
		},
	}

	mntCmd := &cobra.Command{
		Use:   "mount",
		Short: "mount operations in a specific mount namespace",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var mntOpts uint = 0

			fsType := cmd.Flag("type").Value.String()
			mntOptions := cmd.Flag("options").Value.String()
			for _, opt := range strings.Split(mntOptions, ",") {
				opt = strings.TrimSpace(opt)
				switch opt {
				case "ro":
					mntOpts = mntOpts | syscall.MS_RDONLY
				case "bind":
					mntOpts = mntOpts | syscall.MS_BIND
				default:
					return fmt.Errorf("mount option %s is not supported", opt)
				}
			}

			// Ensure that sourceFile is a real path. It will be kept open until used
			// by the syscall via the file descriptor path in proc (SafePath) to ensure
			// that no symlink injection can happen after the check.
			sourceFile, err := NewFileNoFollow(args[0])
			if err != nil {
				return fmt.Errorf("mount source invalid: %v", err)
			}
			defer sourceFile.Close()

			// Ensure that targetFile is a real path. It will be kept open until used
			// by the syscall via the file descriptor path in proc (SafePath) to ensure
			// that no symlink injection can happen after the check.
			targetFile, err := NewFileNoFollow(args[1])
			if err != nil {
				return fmt.Errorf("mount target invalid: %v", err)
			}
			defer targetFile.Close()

			return syscall.Mount(sourceFile.SafePath(), targetFile.SafePath(), fsType, uintptr(mntOpts), "")
		},
	}
	mntCmd.Flags().StringP("options", "o", "", "comma separated list of mount options")
	mntCmd.Flags().StringP("type", "t", "", "fstype")

	umntCmd := &cobra.Command{
		Use:   "umount",
		Short: "unmount in a specific mount namespace",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Ensure that targetFile is a real path. It will be kept open until used
			// by the syscall via the file descriptor path in proc (SafePath) to ensure
			// that no symlink injection can happen after the check.
			targetFile, err := NewPathNoFollow(args[0])
			if err != nil {
				return fmt.Errorf("mount target invalid: %v", err)
			}
			err = targetFile.ExecuteNoFollow(func(safePath string) error {
				// we actively hold an open reference to the mount point,
				// we have to lazy unmount, to not block ourselves
				// with the active file-descriptor.
				return syscall.Unmount(safePath, unix.MNT_DETACH)
			})
			if err != nil {
				return fmt.Errorf("umount failed: %v", err)
			}
			return nil
		},
	}

	rootCmd.AddCommand(
		execCmd,
		mntCmd,
		umntCmd,
	)

	if err := rootCmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
