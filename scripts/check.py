"""Xhunter 的编译与测试检查。

用法：
    python scripts/check.py                 # 本机工具链
    python scripts/check.py --wsl           # 在 WSL 内执行（一期目标平台 linux/amd64）★推荐
    python scripts/check.py --wsl --race    # 额外跑竞态检测
    python scripts/check.py --probe         # 只探测环境，不跑检查

背景与坑：
  - 本机 Windows 侧 bash shim 异常（dirname/grep 不可用），因此直接调 go 可执行文件；
  - 工作区在 WSL 的 ext4 上（/work/agents/xhunter），Windows 侧通过映射盘访问，构建慢一个量级；
  - WSL 的 go 由 **gvm 管理**，只在**交互式** shell 的 PATH 上：`bash -lc` 读不到
    （Ubuntu 的 ~/.bashrc 在非交互时提前 return），所以这里用 `bash -ic` 探测并记住绝对路径；
  - 一期目标平台是 linux/amd64（D6），**运行期验证必须在 WSL 内做**。
"""

import argparse
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile

# 工作目录由脚本位置推导（scripts/check.py 的上一级），而不是硬编码——
# 硬编码会让这份"规范验证入口"换一台机器就跑不了。
WORK_REL = str(pathlib.Path(__file__).resolve().parent.parent)

# 工作目录在 WSL 里的位置。Windows 侧的映射盘（网络盘）在 resolve() 之后会变成 UNC
# （`\\localhost\\work\\…`），而它在 WSL 里是原生挂载路径——两者没有通用的自动换算，
# 所以这里**显式列出**并**逐个验证**：猜不中时明确报错说该配哪一行，而不是把坏路径拼进
# bash 命令（那是「cd: \\localhostwork…」这种看不懂的失败的来源）。
_Q = chr(39)   # 单引号
_BS = chr(92)  # 反斜杠

WSL_PATH_MAP = [
    ("W:\\", "/work"),               # 本机：映射盘 W: 与 WSL 的 /work 是同一份文件
    (r"\\localhost\work", "/work"),  # 同一份文件在 UNC 下的形态
]
WIN_GO_CANDIDATES = [
    r"C:\Users\黄孟柱\.g\go\bin\go.exe",
    r"C:\Program Files\Go\bin\go.exe",
]
WSL_GO_GLOBS = [
    "$HOME/.gvm/gos/*/bin/go",
    "$HOME/.local/go/bin/go",
    "/usr/local/go/bin/go",
    "/usr/lib/go/bin/go",
]
TARGET = ("linux", "amd64")
GO_PATH_RE = re.compile(r"^(/\S*/go)$", re.MULTILINE)


def decode(raw: bytes) -> str:
    """wsl.exe 输出在不同子命令下可能是 UTF-8 或 UTF-16，统一兜底解码。"""
    for enc in ("utf-8", "utf-16-le", "mbcs"):
        try:
            return raw.decode(enc)
        except (UnicodeDecodeError, LookupError):
            continue
    return raw.decode("utf-8", errors="replace")


_Q = chr(39)   # 单引号
_BS = chr(92)  # 反斜杠

def shquote(s: str) -> str:
    """把路径安全地放进 bash 命令：单引号包裹，内部的单引号按 POSIX 规则转义。"""
    return _Q + s.replace(_Q, _Q + _BS + _Q + _Q) + _Q


def to_wsl_path(win_path: str) -> str | None:
    """把 Windows 侧路径换算成 WSL 里的路径；换算不了返回 None。

    先查 WSL_PATH_MAP（映射盘 / UNC），再退化为普通盘符的 /mnt/<drive>。
    换算结果仍然要经 wsl_dir_exists 验证——映射表写错了不该表现为「目录里什么都没有」。
    """
    raw = win_path.replace("/", "\\")
    for win, wsl in WSL_PATH_MAP:
        if raw.upper().startswith(win.upper()):
            rest = raw[len(win):].lstrip("\\").replace("\\", "/")
            base = wsl.rstrip("/")
            return base + ("/" + rest if rest else "") or "/"
    if len(raw) >= 2 and raw[1] == ":":
        rest = raw[2:].lstrip("\\").replace("\\", "/")
        return "/mnt/" + raw[0].lower() + ("/" + rest if rest else "")
    return None


def wsl_dir_exists(path: str) -> bool:
    rc, out, _ = wsl_run(f"test -d {shquote(path)} && echo ok", timeout=120)
    return rc == 0 and "ok" in out


def run(cmd: list[str], env: dict, timeout: int = 1800, cwd: str | None = None):
    try:
        r = subprocess.run(cmd, capture_output=True, env=env, timeout=timeout, cwd=cwd)
        return r.returncode, decode(r.stdout or b"").strip(), decode(r.stderr or b"").strip()
    except subprocess.TimeoutExpired:
        return 124, "", "超时"


def env_for_wsl() -> dict:
    """交给 wsl.exe 的宿主 env。

    两点都是踩出来的：① **空 env 会报 RPC 句柄错误**，所以必须整体传宿主 env；
    ② PATH 里落在映射盘 / UNC 上的项，WSL 换算不了，会刷一屏 `Failed to translate`
    ——它们对 WSL 内的执行没有用处，摘掉即可（只影响继承的 PATH，不影响宿主）。
    """
    env = dict(os.environ)
    sep = ";" if os.name == "nt" else ":"
    def usable(entry: str) -> bool:
        if entry.startswith("\\\\"):
            return False
        up = entry.upper()
        return not any(up.startswith(win.upper().rstrip(_BS)) for win, _ in WSL_PATH_MAP)
    path = env.get("PATH", "")
    env["PATH"] = sep.join(e for e in path.split(sep) if e and usable(e))
    return env


def wsl_run(cmd: str, timeout: int = 1800):
    """在 WSL 内执行 bash 命令。

    cwd 特意挪到系统盘：子进程的 cwd 若落在映射盘上，WSL 会在启动时先报
    `Failed to translate W://…`（只是告警，但会让脚本里的相对路径拿到坏值）。
    """
    cwd = os.environ.get("SystemRoot") or os.environ.get("windir") or "C:////"
    return run(["wsl.exe", "-e", "bash", "-c", cmd], env_for_wsl(), timeout=timeout, cwd=cwd)


def find_native_go() -> str | None:
    found = shutil.which("go")
    if found:
        return found
    for cand in WIN_GO_CANDIDATES:
        if os.path.exists(cand):
            return cand
    return None


def find_wsl_go() -> str | None:
    """定位 WSL 内的 go。gvm 只把 PATH 设进交互式 shell，故先用 bash -ic，再退回常见路径。"""
    if not shutil.which("wsl"):
        return None
    rc, out, _ = wsl_run("bash -ic 'command -v go' 2>/dev/null", timeout=120)
    if rc == 0 and out:
        m = GO_PATH_RE.search(out.strip())
        if m:
            return m.group(1)
    globs = " ".join(WSL_GO_GLOBS)
    rc, out, _ = wsl_run(f"ls -d {globs} 2>/dev/null | head -1", timeout=120)
    return out.strip().splitlines()[0].strip() if rc == 0 and out.strip() else None


def env_for(go: str | None, cross: bool = False) -> dict:
    env = dict(os.environ)
    env["GOFLAGS"] = "-mod=mod"
    if go and go.startswith("/"):
        env["GOCACHE"] = "/tmp/xhunter-gocache"
    else:
        env["GOCACHE"] = os.path.join(tempfile.gettempdir(), "xhunter-gocache")
    if cross:
        env["GOOS"], env["GOARCH"] = TARGET
        env["CGO_ENABLED"] = "0"
    return env


def probe() -> int:
    print("== 环境探测")
    print("  工作目录      ", WORK_REL)
    print("  WSL 工作目录  ", to_wsl_path(WORK_REL) or "未映射（请在 WSL_PATH_MAP 里补一条）")
    print("  调用目录      ", os.getcwd())
    native = find_native_go()
    if native:
        _, out, _ = run([native, "version"], env_for(native), cwd=WORK_REL)
        _, goos, _ = run([native, "env", "GOOS"], env_for(native), cwd=WORK_REL)
        _, arch, _ = run([native, "env", "GOARCH"], env_for(native), cwd=WORK_REL)
        print("  本机 go        ", out or "?")
        print("  本机平台       ", f"{goos}/{arch}")
    else:
        print("  本机 go        未找到")

    wsl_go = find_wsl_go()
    if wsl_go:
        rc, out, _ = wsl_run(f"{wsl_go} version", timeout=120)
        print("  WSL go         ", f"{out or '?'}  ({wsl_go})")
        print("  WSL 平台        linux/amd64 ← 一期目标平台，运行期验证请用 --wsl")
    else:
        print("  WSL go         未找到（可考虑 gvm / 官方 tarball 安装）")
    return 0


def check(use_wsl: bool, race: bool) -> int:
    if use_wsl:
        go = find_wsl_go()
        if not go:
            print("未在 WSL 内找到 go（gvm 需交互式 shell 才上 PATH）", file=sys.stderr)
            return 2
        work = to_wsl_path(WORK_REL)
        if not work or not wsl_dir_exists(work):
            print(
                f"无法把工作目录 {WORK_REL} 换算成 WSL 路径（试出 {work!r}）。\n"
                "请在 scripts/check.py 的 WSL_PATH_MAP 里补一条「Windows 前缀 → WSL 挂载点」映射。",
                file=sys.stderr,
            )
            return 2
        steps = [f"{go} build ./...", f"{go} vet ./...", f"{go} test ./... -count=1"]
        if race:
            steps.append(f"{go} test -race ./... -count=1")
        script = f"cd {shquote(work)} && " + " && ".join(steps)
        rc, out, err = wsl_run(script)
        print(f"== WSL 检查（linux/amd64，一期目标平台）-> exit {rc}")
        if out:
            print(out)
        if err:
            print(err)
        return rc

    go = find_native_go()
    if not go:
        print("未找到 go 工具链：请安装 Go，或用 --wsl 在 WSL 内执行", file=sys.stderr)
        return 2

    steps = [["build", "./..."], ["vet", "./..."], ["test", "./...", "-count=1"]]
    if race:
        steps.append(["test", "-race", "./...", "-count=1"])
    failed = 0
    for args in steps:
        rc, out, err = run([go] + args, env_for(go), cwd=WORK_REL)
        print(f"== go {' '.join(args)} -> exit {rc}")
        if out:
            print(out)
        if err:
            print(err)
        failed |= rc

    # 一期目标是 linux/amd64：本机平台不符时，至少验证目标平台能编译（D6）。
    _, goos, _ = run([go, "env", "GOOS"], env_for(go), cwd=WORK_REL)
    if goos and goos != TARGET[0]:
        rc, out, err = run([go, "build", "./..."], env_for(go, cross=True), cwd=WORK_REL)
        print(f"== 交叉编译 {TARGET[0]}/{TARGET[1]} -> exit {rc}")
        if err:
            print(err)
        failed |= rc
        if rc == 0:
            print("   本机只验证了编译；**运行期行为请在 WSL 内用 --wsl 验证**")
    return 1 if failed else 0


def main() -> int:
    ap = argparse.ArgumentParser(description="Xhunter 编译与测试检查")
    ap.add_argument("--wsl", action="store_true", help="在 WSL 内执行（一期目标平台 linux/amd64）")
    ap.add_argument("--race", action="store_true", help="额外执行竞态检测")
    ap.add_argument("--probe", action="store_true", help="只探测环境")
    args = ap.parse_args()
    if args.probe:
        return probe()
    return check(args.wsl, args.race)


if __name__ == "__main__":
    sys.exit(main())
