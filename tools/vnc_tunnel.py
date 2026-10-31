#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""本地 5901 -> 服务器 127.0.0.1:5901 的 SSH 端口转发。

服务器上的 x11vnc 只监听 127.0.0.1（不暴露公网），所以必须经隧道。
这个脚本在本地监听 5901，把连接透传到服务器。VNC 客户端连 127.0.0.1:5901
即可。

为什么用脚本而不是 `ssh -L`：
    Windows 上 ssh -L 每次都要手输密码，而这个账号是密码登录。
    paramiko 可以在脚本里带上密码，省掉交互。

用法：
    改下面三个常量，然后
    python vnc_tunnel.py

    # 或者用环境变量覆盖，避免把密码写进文件
    set HW_VNC_HOST=1.2.3.4
    set HW_VNC_PASS=yourpassword
    python vnc_tunnel.py
"""
import os
import select
import socketserver

import paramiko

HOST = os.environ.get("HW_VNC_HOST", "YOUR_SERVER_IP")
PORT = int(os.environ.get("HW_VNC_SSH_PORT", "22"))
USER = os.environ.get("HW_VNC_USER", "root")
PASSWORD = os.environ.get("HW_VNC_PASS", "YOUR_SERVER_PASSWORD")
LOCAL_PORT = int(os.environ.get("HW_VNC_LOCAL_PORT", "5901"))
REMOTE_HOST = "127.0.0.1"
REMOTE_PORT = 5901


class Forward(socketserver.BaseRequestHandler):
    def handle(self):
        chan = self.server.ssh_transport.open_channel(
            "direct-tcpip",
            (REMOTE_HOST, REMOTE_PORT),
            self.request.getpeername(),
        )
        if chan is None:
            return
        try:
            while True:
                r, _, _ = select.select([self.request, chan], [], [], 1.0)
                if self.request in r:
                    data = self.request.recv(4096)
                    if not data:
                        break
                    chan.send(data)
                if chan in r:
                    data = chan.recv(4096)
                    if not data:
                        break
                    self.request.send(data)
        finally:
            chan.close()
            self.request.close()


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


def main():
    if "YOUR_SERVER_IP" in HOST:
        print("!! 请先设置 HW_VNC_HOST / HW_VNC_PASS，或改脚本里的常量")
        return 1

    cli = paramiko.SSHClient()
    cli.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    cli.connect(HOST, PORT, USER, PASSWORD, timeout=20)
    print(f"[tunnel] SSH 已连接 {USER}@{HOST}")

    srv = Server(("127.0.0.1", LOCAL_PORT), Forward)
    srv.ssh_transport = cli.get_transport()
    print(f"[tunnel] 本地 127.0.0.1:{LOCAL_PORT} -> "
          f"{HOST}:{REMOTE_HOST}:{REMOTE_PORT}")
    print("[tunnel] 现在用 VNC 客户端连 127.0.0.1:%d" % LOCAL_PORT)
    print("[tunnel] Ctrl+C 断开")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        print("\n[tunnel] 关闭")
    finally:
        srv.server_close()
        cli.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
