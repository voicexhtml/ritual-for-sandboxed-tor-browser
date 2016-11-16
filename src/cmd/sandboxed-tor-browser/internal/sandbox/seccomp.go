// secomp.go - Sandbox seccomp rules.
// Copyright (C) 2016  Yawning Angel.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package sandbox

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"syscall"

	seccomp "github.com/seccomp/libseccomp-golang"

	"cmd/sandboxed-tor-browser/internal/data"
)

func installTBLOzWhitelist(fd *os.File) error {
	defer fd.Close()

	actEPerm := seccomp.ActErrno.SetReturnCode(1)
	f, err := seccomp.NewFilter(actEPerm)
	if err != nil {
		return err
	}
	defer f.Release()
	if err := f.AddArch(seccomp.ArchNative); err != nil {
		return err
	}

	constantTable := map[string]uint64{
		"PR_SET_NAME":       syscall.PR_SET_NAME,
		"PR_GET_NAME":       syscall.PR_GET_NAME,
		"PR_GET_TIMERSLACK": syscall.PR_GET_TIMERSLACK,
		"PR_SET_SECCOMP":    syscall.PR_SET_SECCOMP,
		"AF_UNIX":           syscall.AF_UNIX,
		"AF_INET":           syscall.AF_INET,
		"AF_INET6":          syscall.AF_INET6,
		"AF_NETLINK":        syscall.AF_NETLINK,
	}

	// Load the rule set.
	b, err := data.Asset("torbrowser-launcher-whitelist.seccomp")
	if err != nil {
		return err
	}

	// AFIAK, only certain architectures can use seccomp conditionals that
	// filter based on args.  Fuck x86 anyway.
	canUseConditionals := runtime.GOARCH == "amd64"

	// Parse the rule set and build seccomp rules.
	for ln, l := range bytes.Split(b, []byte{'\n'}) {
		l = bytes.TrimSpace(l)
		if len(l) == 0 { // Empty line.
			continue
		}
		if bytes.HasPrefix(l, []byte{'#'}) { // Comment.
			continue
		}

		if bytes.IndexByte(l, ':') != -1 {
			// Rule
			sp := bytes.SplitN(l, []byte{':'}, 2)
			if len(sp) != 2 {
				return fmt.Errorf("seccomp: invalid rule: %d:%v", ln, string(l))
			}

			scallName := string(bytes.TrimSpace(sp[0]))
			scall, err := seccomp.GetSyscallFromName(scallName)
			if err != nil {
				// Continue instead of failing on ENOSYS.  It's a whitelist.
				// the application will either do without the call, or fail
				// horribly.
				log.Printf("seccomp: unknown system call: %v", scallName)
				continue
			}
			if !canUseConditionals {
				if err = f.AddRule(scall, seccomp.ActAllow); err != nil {
					return err
				}
			} else {
				rawCond := bytes.TrimSpace(sp[1])
				if bytes.Equal(rawCond, []byte{'1'}) {
					if err = f.AddRule(scall, seccomp.ActAllow); err != nil {
						return err
					}
				} else {
					argConds := make([][]uint64, 5)
					conds := bytes.Split(rawCond, []byte{'|', '|'})
					if len(conds) < 1 {
						return fmt.Errorf("seccomp: invalid rule: %d:%v", ln, string(l))
					}
					for _, v := range conds {
						v = bytes.TrimSpace(v)
						spCond := bytes.Split(v, []byte{'=', '='})
						if len(spCond) != 2 {
							return fmt.Errorf("seccomp: invalid condition: %d:%v", ln, string(l))
						}

						arg := string(bytes.TrimSpace(spCond[0]))
						var argN uint
						switch arg {
						case "arg0":
							argN = 0
						case "arg1":
							argN = 1
						case "arg2":
							argN = 2
						case "arg3":
							argN = 3
						case "arg4":
							argN = 4
						case "arg5":
							argN = 5
						default:
							return fmt.Errorf("seccomp: invalid argument: %d:%v", ln, string(l))
						}

						rawVal := string(bytes.TrimSpace(spCond[1]))
						val, ok := constantTable[rawVal]
						if !ok {
							val, err = strconv.ParseUint(rawVal, 0, 64)
							if err != nil {
								return fmt.Errorf("seccomp: invalid value: %d:%v: %v", ln, string(l), err)
							}
						}

						argConds[argN] = append(argConds[argN], val)
					}

					var scConds []seccomp.ScmpCondition
					for arg, vals := range argConds {
						if len(vals) == 0 {
							continue
						}
						for _, val := range vals {
							cond, err := seccomp.MakeCondition(uint(arg), seccomp.CompareEqual, val)
							if err != nil {
								return err
							}
							scConds = append(scConds, cond)
						}
					}

					if err = f.AddRuleConditionalExact(scall, seccomp.ActAllow, scConds); err != nil {
						return err
					}
				}
			}
		} else if bytes.IndexByte(l, '=') != -1 {
			// Declaration.
			sp := bytes.Split(l, []byte{'='})
			if len(sp) != 2 {
				return fmt.Errorf("seccomp: invalid constant: %d:%v", ln, string(l))
			}
			k := string(bytes.TrimSpace(sp[0]))
			v, err := strconv.ParseUint(string(bytes.TrimSpace(sp[1])), 0, 64)
			if err != nil {
				return fmt.Errorf("seccomp: invalid conditional: %d:%v: %v", ln, string(l), err)
			}
			constantTable[k] = v
		} else {
			return fmt.Errorf("seccomp: syntax error in profile: %d:%v", ln, string(l))
		}
	}

	return f.ExportBPF(fd)
}

func installBasicBlacklist(fd *os.File) error {
	defer fd.Close()

	f, err := seccomp.NewFilter(seccomp.ActAllow)
	if err != nil {
		return err
	}
	defer f.Release()
	if err := f.AddArch(seccomp.ArchNative); err != nil {
		return err
	}

	// Install a basic blacklist of calls that should essentially never be
	// allowed, due to potential security/privacy issues.  Processes that
	// require more, should use a whitelist instead.
	actEPerm := seccomp.ActErrno.SetReturnCode(1)
	syscallBlacklist := []string{
		// linux-user-chroot (v0 profile)
		"syslog",      // Block dmesg
		"uselib",      // Useless old syscall
		"personality", // Don't allow you to switch to bsd emulation or whatnot
		"acct",        // Don't allow disabling accounting
		"modify_ldt",  // 16-bit code is unnecessary in the sandbox, and modify_ldt is a historic source of interesting information leaks.
		"quotactl",    // Don't allow reading current quota use

		// Scary VM/NUMA ops:
		"move_pages",
		"mbind",
		"get_mempolicy",
		"set_mempolicy",
		"migrate_pages",

		// Don't allow subnamespace setups:
		// XXX/yawning: The clone restriction breaks bwrap.  c'est la vie.  It
		// looks like Mozilla is considering using user namespaces for the
		// content process sandboxing efforts, so this may need to be enabled.
		"unshare",
		"mount",
		"pivot_root",
		// {SCMP_SYS(clone), &SCMP_A0(SCMP_CMP_MASKED_EQ, CLONE_NEWUSER, CLONE_NEWUSER)}, // Breaks bwrap.

		// Profiling operations; we expect these to be done by tools from
		// outside the sandbox.  In particular perf has been the source of many
		// CVEs.
		"perf_event_open",
		"ptrace",

		// firejail seccomp_filter_64()
		// mount
		"umount2",
		"kexec_load",
		// ptrace
		"open_by_handle_at",
		"name_to_handle_at",
		"create_module",
		"init_module",
		"finit_module",
		"delete_module",
		"iopl",
		"ioperm",
		"ioprio_set",
		"swapon",
		"swapoff",
		// syslog
		"process_vm_readv",
		"process_vm_writev",
		"sysfs",
		"_sysctl",
		"adjtimex",
		"clock_adjtime",
		"lookup_dcookie",
		// perf_event_open
		"fanotify_init",
		"kcmp",
		"add_key",
		"request_key",
		"keyctl",
		// uselib
		// acct
		// modify_ldt
		// pivot_root
		"io_setup",
		"io_destroy",
		"io_getevents",
		"io_submit",
		"io_cancel",
		"remap_file_pages",
		// mbind
		// get_mempolicy
		// set_mempolicy
		// migrate_pages
		// move_pages
		"vmsplice",
		"chroot",
		"tuxcall",
		"reboot",
		"nfsservctl",
		"get_kernel_syms",
	}
	if runtime.GOARCH == "386" {
		syscallBlacklist = append(syscallBlacklist, "vm86", "vm86old")
	}

	for _, n := range syscallBlacklist {
		s, err := seccomp.GetSyscallFromName(n)
		if err != nil {
			return err
		}
		if err := f.AddRule(s, actEPerm); err != nil {
			return err
		}
	}

	// Compile the filter rules, and write it out to the bwrap child process.
	return f.ExportBPF(fd)
}
