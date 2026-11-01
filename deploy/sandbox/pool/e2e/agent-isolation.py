import os,sys,subprocess
# Run in the agent-runner container: it is the agent user already, and nothing here needs more.
print("uid",os.getuid(),"gid",os.getgid())
def t(d,f):
    try: r=f(); print("FAIL-readable" if r is not False else "ok-blocked", d, str(r)[:80])
    except Exception as e: print("ok-blocked", d, type(e).__name__)
t("ls /home/daemon", lambda: os.listdir("/home/daemon"))
t("cat daemon config.json", lambda: open("/home/daemon/.multica/config.json").read())
dp=[p for p in os.listdir("/proc") if p.isdigit() and open(f"/proc/{p}/comm").read().strip()=="multica"]
print("daemon pids (none expected: separate process namespace)",dp)
for p in dp:
    t(f"read /proc/{p}/environ", lambda: open(f"/proc/{p}/environ").read())
    t(f"kill -0 {p}", lambda: os.kill(int(p),0))
t("read /proc/1/environ", lambda: open("/proc/1/environ").read())
t("write /usr/local/x", lambda: open("/usr/local/x","w"))
t("HOME ls", lambda: os.listdir(os.environ.get("HOME","/home/agent")))
t("ls /work/workspaces/.multica", lambda: os.listdir("/work/workspaces/.multica"))
t("ls /work/workspaces/.pending-terminal-reports", lambda: os.listdir("/work/workspaces/.pending-terminal-reports"))
print("cat /etc/multica-sandbox*:", os.path.exists("/etc/multica-sandbox"), os.listdir("/etc/multica-sandbox") if os.path.exists("/etc/multica-sandbox") else "")
