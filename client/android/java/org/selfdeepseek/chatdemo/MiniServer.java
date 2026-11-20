package org.selfdeepseek.chatdemo;

import android.content.Context;
import android.content.res.AssetManager;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.util.HashMap;
import java.util.Locale;
import java.util.Map;

/**
 * 极简静态文件服务：把 APK 里的 assets/ 用 http://127.0.0.1:&lt;port&gt;/ 暴露出来。
 *
 * 之所以不用 file://：WebView 在 file:// 下会限制 localStorage/IndexedDB，
 * 而 chat-demo 把 API Key、会话都放在这两处。
 */
final class MiniServer {

    private final Context context;
    private ServerSocket socket;
    private Thread thread;
    private volatile boolean running;

    private static final Map<String, String> MIME = new HashMap<String, String>();

    static {
        MIME.put("html", "text/html; charset=utf-8");
        MIME.put("htm", "text/html; charset=utf-8");
        MIME.put("js", "application/javascript; charset=utf-8");
        MIME.put("mjs", "application/javascript; charset=utf-8");
        MIME.put("css", "text/css; charset=utf-8");
        MIME.put("json", "application/json; charset=utf-8");
        MIME.put("svg", "image/svg+xml");
        MIME.put("png", "image/png");
        MIME.put("jpg", "image/jpeg");
        MIME.put("jpeg", "image/jpeg");
        MIME.put("webp", "image/webp");
        MIME.put("gif", "image/gif");
        MIME.put("ico", "image/x-icon");
        MIME.put("woff", "font/woff");
        MIME.put("woff2", "font/woff2");
        MIME.put("ttf", "font/ttf");
        MIME.put("otf", "font/otf");
        MIME.put("wasm", "application/wasm");
        MIME.put("txt", "text/plain; charset=utf-8");
        MIME.put("map", "application/json; charset=utf-8");
    }

    MiniServer(Context context) {
        this.context = context.getApplicationContext();
    }

    /** 固定端口：WebView 的 localStorage / IndexedDB 是按 origin（含端口）隔离的，
     *  端口每次都变会导致登录状态和聊天记录在每次启动时丢失。 */
    private static final int PREFERRED_PORT = 18888;

    /**
     * 启动并返回端口；失败返回 -1。
     * 端口必须尽量固定：origin 变了 localStorage 就换一套，登录态会丢。
     * 所以占位时先重试若干次（上一次进程可能还在收尾），实在不行才退回随机端口。
     */
    int start() {
        try {
            for (int i = 0; i < 10; i++) {
                try {
                    socket = new ServerSocket(PREFERRED_PORT, 16, InetAddress.getByName("127.0.0.1"));
                    break;
                } catch (Throwable busy) {
                    if (i == 9) {
                        // 仍被占用：退回随机端口，代价是该次启动的登录态不保留
                        socket = new ServerSocket(0, 16, InetAddress.getByName("127.0.0.1"));
                        break;
                    }
                    try {
                        Thread.sleep(200);
                    } catch (InterruptedException ignored) {
                    }
                }
            }
            final int port = socket.getLocalPort();
            running = true;
            thread = new Thread(new Runnable() {
                @Override
                public void run() {
                    loop();
                }
            }, "mini-http");
            thread.setDaemon(true);
            thread.start();
            return port;
        } catch (Throwable t) {
            return -1;
        }
    }

    void stop() {
        running = false;
        try {
            if (socket != null) {
                socket.close();
            }
        } catch (Throwable ignored) {
        }
    }

    private void loop() {
        while (running) {
            Socket client;
            try {
                client = socket.accept();
            } catch (Throwable t) {
                break;
            }
            try {
                handle(client);
            } catch (Throwable ignored) {
            } finally {
                try {
                    client.close();
                } catch (Throwable ignored) {
                }
            }
        }
    }

    private void handle(Socket client) throws IOException {
        InputStream in = client.getInputStream();
        ByteArrayOutputStream head = new ByteArrayOutputStream();
        int prev = 0;
        // 只读请求行 + 头，遇到空行结束
        while (true) {
            int c = in.read();
            if (c < 0) {
                return;
            }
            if (prev == '\r' && c == '\n') {
                break;
            }
            if (head.size() < 8192) {
                head.write(c);
            }
            prev = c;
        }
        String requestLine = new String(head.toByteArray(), "ISO-8859-1").split("\r\n")[0];
        String[] parts = requestLine.split(" ");
        if (parts.length < 2) {
            respond(client.getOutputStream(), 400, "text/plain", "bad request".getBytes("UTF-8"));
            return;
        }
        String path = parts[1];
        int q = path.indexOf('?');
        if (q >= 0) {
            path = path.substring(0, q);
        }
        if (path.equals("/") || path.isEmpty()) {
            path = "/index.html";
        }
        String asset = path.startsWith("/") ? path.substring(1) : path;
        byte[] body = readAsset(asset);
        String type = mimeOf(asset);
        if (body == null && !asset.contains(".")) {
            // 前端路由回退到 index.html
            body = readAsset("index.html");
            type = MIME.get("html");
        }
        if (body == null) {
            respond(client.getOutputStream(), 404, "text/plain", "not found".getBytes("UTF-8"));
            return;
        }
        respond(client.getOutputStream(), 200, type, body);
    }

    private byte[] readAsset(String name) {
        AssetManager am = context.getAssets();
        InputStream is = null;
        try {
            is = am.open(name);
            ByteArrayOutputStream bos = new ByteArrayOutputStream();
            byte[] buf = new byte[16384];
            int n;
            while ((n = is.read(buf)) > 0) {
                bos.write(buf, 0, n);
            }
            return bos.toByteArray();
        } catch (Throwable t) {
            return null;
        } finally {
            if (is != null) {
                try {
                    is.close();
                } catch (Throwable ignored) {
                }
            }
        }
    }

    private String mimeOf(String name) {
        int dot = name.lastIndexOf('.');
        if (dot < 0) {
            return "application/octet-stream";
        }
        String ext = name.substring(dot + 1).toLowerCase(Locale.US);
        String t = MIME.get(ext);
        return t != null ? t : "application/octet-stream";
    }

    private void respond(OutputStream out, int code, String type, byte[] body) throws IOException {
        String status = code == 200 ? "OK" : (code == 404 ? "Not Found" : "Bad Request");
        StringBuilder sb = new StringBuilder();
        sb.append("HTTP/1.1 ").append(code).append(' ').append(status).append("\r\n");
        sb.append("Content-Type: ").append(type).append("\r\n");
        sb.append("Content-Length: ").append(body.length).append("\r\n");
        sb.append("Cache-Control: no-store\r\n");
        sb.append("Connection: close\r\n\r\n");
        out.write(sb.toString().getBytes("ISO-8859-1"));
        out.write(body);
        out.flush();
    }
}
