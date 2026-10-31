#!/usr/bin/env python3
"""把本机的会话文件同步到服务器。

只有 cookie 文件真正变化时才传输，避免无谓覆盖。
同步后清理服务器上的 NEED_MANUAL_LOGIN 标记。
"""
import hashlib
import os
import sys

import paramiko

HOST = "<SERVER_IP>"
USER = "root"
PASS = "D<ACCOUNT_PASSWORD>"
REMOTE_STATE = "/opt/hwcloud-demo/state"

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FILES = ["cookies.json", "meta.json"]


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


def main():
    c = paramiko.SSHClient()
    c.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    try:
        c.connect(HOST, username=USER, password=PASS, timeout=20)
    except Exception as e:
        print(f"[!] 连接失败: {e}")
        return 1

    try:
        sftp = c.open_sftp()
        for name in FILES:
            local = os.path.join(ROOT, "state", name)
            remote = f"{REMOTE_STATE}/{name}"
            if not os.path.exists(local):
                print(f"[!] 本地缺少 {local}")
                return 1

            try:
                remote_hash = sftp.stat(remote).st_size  # 只做存在性检查
            except IOError:
                remote_hash = None

            sftp.put(local, remote)
            print(f"[+] 已上传 {name} (sha256={sha256(local)[:16]}...)")
        sftp.close()

        # 权限收紧 + 清理失败标记
        for cmd in [
            f"chmod 600 {REMOTE_STATE}/cookies.json {REMOTE_STATE}/meta.json",
            f"rm -f {REMOTE_STATE}/NEED_MANUAL_LOGIN",
        ]:
            c.exec_command(cmd, timeout=30)[1].read()

        # 验证服务器端会话
        _, out, _ = c.exec_command("/usr/local/bin/hwcloud session", timeout=90)
        result = out.read().decode("utf-8", "replace")
        print("\n服务器会话状态：")
        print(result.rstrip())
        if '"alive": true' not in result:
            print("[!] 服务器端会话仍无效")
            return 1
        return 0
    finally:
        c.close()


if __name__ == "__main__":
    sys.exit(main())
