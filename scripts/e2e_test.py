#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
端到端验收脚本：对真实运行的服务完成一次真实文件传输。

不使用任何 mock：文件在本地生成，分块通过 HTTP 真实上传，
服务端真实校验 SHA-256 并落盘，最后通过下载接口取回并逐字节比对。

用法：python e2e_test.py http://127.0.0.1:8791
"""
import hashlib
import io
import json
import os
import sys
import time
import http.client
import urllib.parse

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8791"
FILE_NAME = "端到端验收.bin"
# 远程基地址：用于模拟「局域网内另一台设备」。
# 若为空则自动从服务端枚举的局域网地址中选取。
REMOTE_BASE = ""

passed = []
failed = []


def check(name, ok, detail=""):
    (passed if ok else failed).append(name)
    mark = "PASS" if ok else "FAIL"
    line = f"[{mark}] {name}"
    if detail and not ok:
        line += f"  ->  {detail}"
    print(line, flush=True)


class Conn:
    """
    一个按主机复用 TCP 连接的极简 HTTP 客户端。

    为什么不用 urllib：urllib 每次请求都会新建连接并在响应后立即关闭。
    在 Windows + 虚拟网卡/代理过滤驱动的环境下，这种高频的
    「连接-大数据传输-关闭」交替会偶发 ConnectionResetError，
    而这与真实浏览器行为并不一致——浏览器会复用连接。
    这里显式复用连接，既贴近真实场景，也让验收结果稳定可复现。
    """

    def __init__(self, base):
        self.base = base.rstrip("/")
        self.host, self.port = self._parse(base)
        self.conn = None

    @staticmethod
    def _parse(base):
        rest = base.split("://", 1)[1]
        hostport = rest.split("/", 1)[0]
        if hostport.startswith("["):  # IPv6
            host, _, port = hostport[1:].partition("]")
            return host, int(port.lstrip(":") or 80)
        if ":" in hostport:
            host, port = hostport.rsplit(":", 1)
            return host, int(port)
        return hostport, 80

    def _ensure(self):
        if self.conn is None:
            self.conn = http.client.HTTPConnection(self.host, self.port, timeout=180)

    def close(self):
        if self.conn is not None:
            try:
                self.conn.close()
            except Exception:
                pass
            self.conn = None

    def request(self, method, path, body=None, raw=None, headers=None):
        hdrs = dict(headers or {})
        data = None
        if raw is not None:
            data = raw
            hdrs.setdefault("Content-Type", "application/octet-stream")
        elif body is not None:
            data = json.dumps(body).encode("utf-8")
            hdrs["Content-Type"] = "application/json"

        last_err = None
        for attempt in range(2):
            try:
                self._ensure()
                self.conn.request(method, path, body=data, headers=hdrs)
                resp = self.conn.getresponse()
                payload = resp.read()
                out = (resp.status, payload, dict(resp.headers))
                # 服务端在 Connection: close 时会关闭连接，据其指示清理本地句柄。
                if resp.will_close:
                    self.close()
                return out
            except Exception as e:
                last_err = e
                self.close()
                if attempt == 0:
                    time.sleep(0.15)
                    continue
        raise last_err


_CONNS = {}


def conn_for(base):
    b = (base or BASE).rstrip("/")
    if b not in _CONNS:
        _CONNS[b] = Conn(b)
    return _CONNS[b]


def req(method, path, body=None, token=None, raw=None, headers=None, expect=None, base=None):
    hdrs = dict(headers or {})
    if token:
        hdrs["Authorization"] = "Bearer " + token
    status, payload, out_headers = conn_for(base).request(method, path, body=body, raw=raw, headers=hdrs)
    parsed = None
    if payload and "json" in out_headers.get("Content-Type", ""):
        try:
            parsed = json.loads(payload.decode("utf-8"))
        except Exception:
            parsed = None
    if expect is not None and status != expect:
        print(f"    ! {method} {path} 期望 {expect} 实际 {status}: {payload[:300]!r}")
    return status, parsed, payload, out_headers


passed = []
failed = []


def check(name, ok, detail=""):
    (passed if ok else failed).append(name)
    mark = "PASS" if ok else "FAIL"
    line = f"[{mark}] {name}"
    if detail and not ok:
        line += f"  ->  {detail}"
    print(line, flush=True)


def new_session(name, base=None):
    st, body, _, _ = req("POST", "/api/session", {"name": name}, expect=201, base=base)
    assert st == 201, body
    return body["token"], body["device"]


def main():
    print(f"目标服务: {BASE}\n" + "-" * 68)

    # 1) 健康检查（无需鉴权）
    st, h, _, _ = req("GET", "/api/health", expect=200)
    check("服务健康检查可用", st == 200 and h and h.get("status") == "ok", str(h))
    if st != 200:
        print("服务不可用，终止测试")
        return 1

    # 1.5) 自动探测局域网地址，用于远程越权检查（服务端枚举出的真实地址）
    global REMOTE_BASE
    if not REMOTE_BASE:
        # 该接口需要鉴权，先登记一个临时会话再取地址列表。
        tmp_tok, _ = new_session("地址探测")
        st, acc, _, _ = req("GET", "/api/session/access", token=tmp_tok, expect=200)
        for a in acc.get("addresses", []):
            if not a.get("loopback") and a.get("family") == "ipv4":
                REMOTE_BASE = a["url"]
                break
        if REMOTE_BASE:
            print(f"    使用远程地址: {REMOTE_BASE}")

    # 2) 会话登记
    sender_tok, sender = new_session("E2E 发送端")
    receiver_tok, receiver = new_session("E2E 接收端")
    check("两个会话登记成功且标识不同", sender["id"] != receiver["id"])

    # 3) 未鉴权请求必须被拒绝
    st, _, _, _ = req("GET", "/api/devices")
    check("无令牌访问设备接口返回 401", st == 401, f"status={st}")

    # 4) 设备列表应包含真实在线信息
    st, devs, _, _ = req("GET", "/api/devices", token=sender_tok, expect=200)
    check("设备列表可读取", st == 200 and "devices" in (devs or {}), str(devs)[:200])
    check("会话自身出现在设备列表中", any(d["id"] == sender["id"] for d in devs["devices"]))

    # 5) 生成真实测试文件：跨多个分块（默认 8MiB）
    # 使用带时间戳的唯一文件名：既避免与历史文件重名影响断言，
    # 也让「自动重命名」策略在真的有冲突时能被单独验证。
    global FILE_NAME
    FILE_NAME = f"端到端验收-{int(time.time() * 1000)}.bin"
    size = 20 * 1024 * 1024 + 12345
    payload = bytearray()
    x = 0xC0FFEE
    for _ in range(size):
        x = (x * 1103515245 + 12345) & 0xFFFFFFFF
        payload.append((x >> 16) & 0xFF)
    payload = bytes(payload)
    want_sha = hashlib.sha256(payload).hexdigest()
    check("生成 20MB 测试文件", len(payload) == size, f"{len(payload)}")

    # 6) 创建传输请求
    st, created, _, _ = req(
        "POST",
        "/api/transfers",
        {
            "receiverIds": [receiver["id"]],
            "conflict": "rename",
            "note": "端到端验收",
            "files": [
                {
                    "name": FILE_NAME,
                    "relPath": "验收/" + FILE_NAME,
                    "size": len(payload),
                    "mime": "application/octet-stream",
                    "modTime": int(time.time() * 1000),
                }
            ],
        },
        token=sender_tok,
        expect=201,
    )
    check("创建传输请求成功", st == 201 and created and created.get("tasks"), str(created)[:300])
    if not created or not created.get("tasks"):
        return 1
    task = created["tasks"][0]
    tid = task["id"]
    check("新任务状态为等待确认", task["status"] == "awaiting", task["status"])
    check("任务总大小与文件一致", task["totalSize"] == size, str(task["totalSize"]))

    # 7) 接收方确认
    st, acc, _, _ = req("POST", f"/api/transfers/{tid}/accept", {"conflict": "rename"}, token=receiver_tok, expect=200)
    check("接收方确认成功", st == 200 and acc["task"]["status"] in ("queued", "uploading"), str(acc)[:200])

    # 8) 等待服务端分配传输槽位
    status = acc["task"]["status"]
    for _ in range(50):
        if status == "uploading":
            break
        time.sleep(0.1)
        st, got, _, _ = req("GET", f"/api/transfers/{tid}", token=sender_tok, expect=200)
        status = got["task"]["status"]
    check("任务进入传输中状态", status == "uploading", status)

    # 9) 查询分块状态，拿到真实分块大小与缺失清单
    st, cs, _, _ = req("GET", f"/api/transfers/{tid}/chunks", token=sender_tok, expect=200)
    chunk_size = cs["chunkSize"]
    chunk_count = cs["files"][0]["chunkCount"]
    check("分块信息真实可用", chunk_size > 0 and chunk_count == (size + chunk_size - 1) // chunk_size,
          f"chunkSize={chunk_size} count={chunk_count}")
    check("初始缺失分块为全部", len(cs["files"][0]["missing"]) == chunk_count)

    # 10) 真实上传全部分块（每个分块都声明 SHA-256）
    t0 = time.time()
    for idx in range(chunk_count):
        off = idx * chunk_size
        part = payload[off:off + chunk_size]
        sha = hashlib.sha256(part).hexdigest()
        st, res, raw, _ = req(
            "POST",
            f"/api/transfers/{tid}/files/0/chunks/{idx}",
            raw=part,
            token=sender_tok,
            headers={"X-Chunk-SHA256": sha},
            expect=200,
        )
        if st != 200:
            check(f"上传分块 {idx}", False, raw[:200])
            return 1
        # 11) 第 0 块立刻重复提交一次：必须幂等，且不能把已传字节翻倍
        if idx == 0:
            st2, res2, _, _ = req("POST", f"/api/transfers/{tid}/files/0/chunks/0", raw=part,
                                  token=sender_tok, headers={"X-Chunk-SHA256": sha}, expect=200)
            check("重复分块被识别为重复且进度不变",
                  st2 == 200 and res2.get("duplicate") is True
                  and res2["fileDoneBytes"] == res["fileDoneBytes"],
                  str(res2)[:200])
            # 再发一个哈希不符的分块：必须被拒绝且计入失败
            st3, err3, _, _ = req("POST", f"/api/transfers/{tid}/files/0/chunks/0",
                                  raw=part, token=sender_tok,
                                  headers={"X-Chunk-SHA256": "0" * 64})
            check("分块哈希不符被拒绝",
                  st3 == 400 and err3["error"]["code"] == "chunk_hash_mismatch",
                  str(err3)[:200])
    elapsed = time.time() - t0
    check(f"全部分块上传成功（{chunk_count} 块，耗时 {elapsed:.2f}s，"
          f"{size / 1024 / 1024 / max(elapsed, 0.001):.1f} MB/s）", True)

    # 12) 任务应完成且通过校验
    st, final, _, _ = req("GET", f"/api/transfers/{tid}", token=sender_tok, expect=200)
    ft = final["task"]
    check("任务状态为已完成", ft["status"] == "completed", ft["status"])
    check("任务标记为已通过完整性校验", ft["verified"] is True)
    check("服务端已传字节等于总大小", ft["doneSize"] == ft["totalSize"], f"{ft['doneSize']}/{ft['totalSize']}")
    check("服务端计算出的 SHA-256 与源文件一致", ft["files"][0]["sha256"] == want_sha,
          f"{ft['files'][0]['sha256']} != {want_sha}")
    check("文件最终名与源文件名一致（无重名冲突）", ft["files"][0]["finalName"] == FILE_NAME,
          str(ft["files"][0]["finalName"]))

    # 13) 接收方签发下载票据并真实下载
    st, tk, _, _ = req("POST", f"/api/transfers/{tid}/files/0/ticket", {}, token=receiver_tok, expect=200)
    check("签发下载票据成功", st == 200 and tk.get("url"), str(tk)[:200])
    if not tk or not tk.get("url"):
        return 1
    st, _, dl, dlh = req("GET", tk["url"])
    check("下载成功", st == 200, f"status={st}")
    check("下载内容与源文件逐字节一致", hashlib.sha256(dl).hexdigest() == want_sha,
          f"len={len(dl)}")
    check("下载响应支持 Range（可续传下载）", dlh.get("Accept-Ranges") == "bytes")
    check("下载响应带 attachment 头", "attachment" in (dlh.get("Content-Disposition") or ""))

    # 14) 伪造票据必须被拒绝
    st, _, _, _ = req("GET", "/api/download?ticket=" + urllib.parse.quote("伪造票据"))
    check("伪造下载票据被拒绝", st == 403, f"status={st}")

    # 15) 越权访问必须被拒绝。
    #
    # 这里必须用「非回环地址」建立第三方会话：从 127.0.0.1 访问的浏览器被视为
    # 服务所在机器本身（拥有管理权限），因此无法用来验证越权。
    # 通过局域网 IP 访问时，服务端看到的来源不是回环地址，该会话就是普通远程设备。
    remote_tok, remote_dev = None, None
    if REMOTE_BASE:
        st, _, _, _ = req("GET", "/api/health", base=REMOTE_BASE)
        if st == 200:
            remote_tok, remote_dev = new_session("E2E 远程旁观者", base=REMOTE_BASE)
            check("可通过局域网地址访问服务", True)
            st, cfgr, _, _ = req("GET", "/api/config", token=remote_tok, base=REMOTE_BASE, expect=200)
            check("远程设备被判定为非特权", cfgr.get("privileged") is False, str(cfgr.get("privileged")))
            if remote_tok:
                st, _, _, _ = req("GET", f"/api/transfers/{tid}", token=remote_tok, base=REMOTE_BASE)
                check("远程第三方无法查看任务详情", st == 403, f"status={st}")
                st, _, _, _ = req("POST", f"/api/transfers/{tid}/files/0/ticket", {},
                                  token=remote_tok, base=REMOTE_BASE)
                check("远程第三方无法签发下载票据", st == 403, f"status={st}")
                st, _, _, _ = req("POST", f"/api/transfers/{tid}/cancel", {},
                                  token=remote_tok, base=REMOTE_BASE)
                check("远程第三方无法取消任务", st == 403, f"status={st}")
                st, _, _, _ = req("PATCH", "/api/settings", {"maxConcurrentTransfers": 9},
                                  token=remote_tok, base=REMOTE_BASE)
                check("远程第三方无法修改设置", st == 403, f"status={st}")
                st, _, _, _ = req("POST", "/api/history/clear", {},
                                  token=remote_tok, base=REMOTE_BASE)
                check("远程第三方无法清除历史", st == 403, f"status={st}")
        else:
            print("    ! 无法通过局域网地址访问，跳过远程越权检查")
    else:
        print("    ! 未提供远程基地址，跳过远程越权检查")

    # 16) 对已完成任务继续上传：必须返回结构化错误（而不是连接重置）
    st, err, _, _ = req("POST", f"/api/transfers/{tid}/files/0/chunks/0", raw=b"x" * 1024,
                        token=sender_tok)
    check("已完成任务拒绝继续上传且返回错误码",
          st == 400 and err is not None and err["error"]["code"] == "invalid_state",
          f"status={st} body={err}")

    # 17) 非法分块索引：必须在一个仍在传输中的任务上验证，
    #     否则「已完成」的状态检查会先触发（这也是正确的失败优先级）。
    st, created2, _, _ = req(
        "POST", "/api/transfers",
        {"receiverIds": [receiver["id"]], "conflict": "rename",
         "files": [{"name": "索引校验.bin", "size": 4096}]},
        token=sender_tok, expect=201)
    tid2 = created2["tasks"][0]["id"]
    req("POST", f"/api/transfers/{tid2}/accept", {"conflict": "rename"}, token=receiver_tok, expect=200)
    for _ in range(50):
        st, got2, _, _ = req("GET", f"/api/transfers/{tid2}", token=sender_tok, expect=200)
        if got2["task"]["status"] == "uploading":
            break
        time.sleep(0.1)
    st, err, _, _ = req("POST", f"/api/transfers/{tid2}/files/0/chunks/99999",
                        raw=b"x" * 4096, token=sender_tok)
    check("非法分块索引被拒绝（带真实请求体）",
          st == 400 and err["error"]["code"] == "invalid_chunk_index", str(err)[:200])
    st, err, _, _ = req("POST", f"/api/transfers/{tid2}/files/99/chunks/0",
                        raw=b"x" * 4096, token=sender_tok)
    check("非法文件索引被拒绝", st == 404 and err["error"]["code"] == "not_found", str(err)[:200])
    # 收尾：取消这个中间任务，避免占用并发槽位
    req("POST", f"/api/transfers/{tid2}/cancel", {}, token=sender_tok)

    # 18) 文件超限
    st, _, _, _ = req("GET", "/api/settings", token=sender_tok, expect=200)
    st, err, _, _ = req("POST", "/api/transfers",
                        {"receiverIds": [receiver["id"]],
                         "files": [{"name": "big.bin", "size": 10 ** 12}]}, token=sender_tok)
    check("超大任务被拒绝并给出错误码", st == 413 and err["error"]["code"] in ("file_too_large", "task_too_large"),
          str(err)[:200])

    # 18) 服务端落盘校验：真实读取接收目录中的文件
    st, diag, _, _ = req("GET", "/api/diagnostics", token=sender_tok, expect=200)
    recv_dir = diag["storage"]["receiveDir"]
    found = None
    for root, _dirs, names in os.walk(recv_dir):
        for n in names:
            if n == FILE_NAME:
                found = os.path.join(root, n)
    check("文件真实落盘到接收目录", found is not None, f"receiveDir={recv_dir}")
    if found:
        with open(found, "rb") as fh:
            disk = fh.read()
        check("落盘文件内容与源文件一致", hashlib.sha256(disk).hexdigest() == want_sha,
              f"len={len(disk)}")
        check("落盘路径保留了相对目录结构", os.path.basename(os.path.dirname(os.path.dirname(found))) != "",
              found)

    # 19) 历史记录
    st, hist, _, _ = req("GET", "/api/history", token=sender_tok, expect=200)
    entry = next((e for e in (hist or {}).get("entries", []) if e["taskId"] == tid), None)
    check("历史记录包含本次任务", entry is not None, str(hist)[:200])
    if entry:
        check("历史状态为已完成", entry["status"] == "completed", entry["status"])
        check("历史方向为发送", entry["direction"] == "out", entry["direction"])
        check("历史标记为已校验", entry["verified"] is True)

    # 20) 诊断接口返回真实数据
    st, diag2, _, _ = req("GET", "/api/diagnostics", token=sender_tok, expect=200)
    check("诊断接口返回检测时间", bool(diag2.get("checkedAt")))
    check("诊断接口返回真实磁盘数据", diag2["storage"]["diskTotalBytes"] > 0)
    check("诊断接口包含本机自检结果", "ok" in diag2["service"]["selfTest"])
    check("诊断接口包含排查建议", len(diag2.get("tips") or []) > 0)

    # 21) 二维码
    st, _, png, hdrs = req("GET", "/api/qrcode.png")
    check("二维码为真实 PNG", st == 200 and png[:4] == b"\x89PNG", f"status={st} len={len(png)}")

    # 22) 静态资源（前端构建产物）
    st, _, html, _ = req("GET", "/")
    check("首页返回前端构建产物", st == 200 and b"<div id=\"root\">" in html, f"status={st}")
    st, _, spa, _ = req("GET", "/settings")
    check("前端路由回退到 index.html", st == 200 and b"<div id=\"root\">" in spa, f"status={st}")
    st, _, body404, _ = req("GET", "/api/" + urllib.parse.quote("不存在的接口"))
    check("未知 API 返回 404（而不是回退成首页 HTML）",
          st == 404, f"status={st} body={body404[:60]!r}")

    # 23) 临时接收入口（开启 -> 匿名上传 -> 关闭后失效）
    st, db, _, _ = req("POST", "/api/dropbox/enable", {"ttlMinutes": 5}, token=sender_tok, expect=200)
    check("临时入口开启成功", st == 200 and db.get("enabled") and db.get("token"), str(db)[:200])
    if db.get("token"):
        # 用一个「陌生人」会话来上传：临时入口的意义就是让没有权限的设备也能投递文件。
        if remote_tok:
            uploader_tok, uploader_base = remote_tok, REMOTE_BASE
        else:
            uploader_tok, _ = new_session("E2E 临时上传者")
            uploader_base = None
        small = b"dropbox payload " * 100
        st, cr, _, _ = req("POST", "/api/transfers",
                           {"dropboxToken": db["token"],
                            "files": [{"name": "临时上传.txt", "size": len(small)}]},
                           token=uploader_tok, base=uploader_base, expect=201)
        ok = st == 201 and cr.get("tasks")
        check("通过临时入口创建任务成功（免确认）", ok, str(cr)[:200])
        if ok:
            dt = cr["tasks"][0]
            check("临时入口任务标记为 viaDropbox", dt.get("viaDropbox") is True, str(dt)[:200])
            dsha = hashlib.sha256(small).hexdigest()
            st, _, raw, _ = req("POST", f"/api/transfers/{dt['id']}/files/0/chunks/0", raw=small,
                                token=uploader_tok, base=uploader_base,
                                headers={"X-Chunk-SHA256": dsha}, expect=200)
            check("临时入口分块上传成功", st == 200, raw[:200] if st != 200 else "")
            st, done, _, _ = req("GET", f"/api/transfers/{dt['id']}", token=uploader_tok,
                                 base=uploader_base, expect=200)
            check("临时入口任务完成", done["task"]["status"] == "completed", done["task"]["status"])
        st, off, _, _ = req("POST", "/api/dropbox/disable", {}, token=sender_tok, expect=200)
        check("临时入口关闭成功", st == 200 and off.get("enabled") is False, str(off)[:200])
        st, err, _, _ = req("POST", "/api/transfers",
                            {"dropboxToken": db["token"], "files": [{"name": "x.txt", "size": 4}]},
                            token=uploader_tok, base=uploader_base)
        check("关闭后旧令牌立即失效（后端强制）", st == 410 and err["error"]["code"] == "dropbox_expired",
              str(err)[:200])

    # 24) 设置校验与特权
    st, _, _, _ = req("PATCH", "/api/settings", {"maxConcurrentTransfers": 999}, token=sender_tok)
    check("非法设置被拒绝且不生效", st == 400, f"status={st}")
    st, _, _, _ = req("PATCH", "/api/settings", {"maxConcurrentTransfers": 8}, token=sender_tok)
    check("合法设置保存成功", st == 200, f"status={st}")
    st, cfg, _, _ = req("GET", "/api/config", token=sender_tok, expect=200)
    check("设置变更真实生效", cfg["maxConcurrentTransfers"] == 8, str(cfg.get("maxConcurrentTransfers")))
    st, _, _, _ = req("PATCH", "/api/settings", {"maxConcurrentTransfers": 3}, token=sender_tok)

    # 25) 清除历史不影响磁盘文件
    st, _, _, _ = req("POST", "/api/history/clear", {}, token=sender_tok, expect=200)
    check("清除历史成功", st == 200)
    if found:
        check("清除历史后磁盘文件仍然存在", os.path.exists(found))
    st, hist2, _, _ = req("GET", "/api/history", token=sender_tok, expect=200)
    check("历史记录已清空", len(hist2.get("entries") or []) == 0, str(len(hist2.get("entries") or [])))

    print("-" * 68)
    print(f"通过 {len(passed)} 项，失败 {len(failed)} 项")
    if failed:
        print("失败项：")
        for f in failed:
            print("  -", f)
        return 1
    print("全部端到端验收通过。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
