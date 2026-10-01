# 执行器演示脚本：由 configs/config.yaml 的 executors.commands.py_hello 档位调用。
# 命令行由档位的 args_render 拼出：python <本文件> --day=<yesterday|today> --fail=<0|1>。
import os
import sys

# 输出被管道接走时，Python 按系统本地编码写（中文 Windows 是 cp936），
# 而执行器产物与 Web 控制台都按 UTF-8 读，这里显式统一。
sys.stdout.reconfigure(encoding="utf-8")
sys.stderr.reconfigure(encoding="utf-8")


def main(argv):
    flags = {}
    for arg in argv:
        if arg.startswith("--") and "=" in arg:
            name, value = arg[2:].split("=", 1)
            flags[name] = value

    print("hello from godelayq executor")
    print(f"runtime=python {sys.version.split()[0]}")
    print(f"cwd={os.getcwd()}")
    print(f"day={flags.get('day', '<missing>')}")
    print(f"argv={argv}")
    print(f"env_count={len(os.environ)}")
    for name in ("PATH", "LANG", "LC_ALL", "TZ", "HOME"):
        state = "set" if os.environ.get(name) else "unset"
        print(f"env {name}={state}")

    if flags.get("fail") == "1":
        print("failing on purpose: exit 75, which retry_on_exit allows", file=sys.stderr)
        return 75
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
