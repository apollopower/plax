package portpool

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

func ProbeFree(port int) bool {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// PortOwner returns the PID and command name of the process bound to port.
// Linux-only: reads /proc/net/tcp and /proc/<pid>/. Returns ok=false on
// other platforms. ProbeFree is the portable alternative.
func PortOwner(port int) (pid int, cmdline string, ok bool) {
	hexPort := fmt.Sprintf("%04X", port)
	inode := findInode(hexPort)
	if inode == 0 {
		return 0, "", false
	}
	pid = findPIDByInode(inode)
	if pid == 0 {
		return 0, "", false
	}
	cmdline = readCmdline(pid)
	return pid, cmdline, true
}

func findInode(hexPort string) uint64 {
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan()
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 10 {
				continue
			}
			st := fields[3]
			if st != "0A" {
				continue
			}
			local := fields[1]
			idx := strings.LastIndex(local, ":")
			if idx < 0 {
				continue
			}
			if strings.EqualFold(local[idx+1:], hexPort) {
				ino, err := strconv.ParseUint(fields[9], 10, 64)
				if err != nil {
					continue
				}
				_ = f.Close()
				return ino
			}
		}
		if err := sc.Err(); err != nil {
			_ = f.Close()
			continue
		}
		_ = f.Close()
	}
	return 0
}

func findPIDByInode(target uint64) int {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, entry := range procs {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fdDir := fmt.Sprintf("/proc/%d/fd", pid)
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fdEntry := range fds {
			link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fdEntry.Name()))
			if err != nil {
				continue
			}
			if !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inoStr := link[len("socket:[") : len(link)-1]
			ino, err := strconv.ParseUint(inoStr, 10, 64)
			if err != nil {
				continue
			}
			if ino == target {
				return pid
			}
		}
	}
	return 0
}

func readCmdline(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	fields := strings.FieldsFunc(string(data), func(r rune) bool { return r == 0 })
	return strings.Join(fields, " ")
}
