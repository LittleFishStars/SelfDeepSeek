package org.selfdeepseek.chatdemo;

import android.app.Activity;
import android.app.AlertDialog;
import android.app.ProgressDialog;
import android.content.Context;
import android.content.DialogInterface;
import android.content.Intent;
import android.content.pm.PackageInfo;
import android.net.Uri;
import android.os.Build;
import android.os.Handler;
import android.os.Looper;
import android.provider.Settings;
import android.widget.Toast;

import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;

/**
 * 应用自动更新。
 *
 * 清单在下载站：https://download.example.com/deepseek/latest.json
 *
 * <pre>
 * {
 *   "version": "1.4.9",
 *   "versionCode": 73,
 *   "notes": "改了什么",
 *   "platforms": {
 *     "android": { "versionCode": 73, "url": "...apk", "size": 2030845, "sha256": "..." }
 *   }
 * }
 * </pre>
 *
 * 设计取舍：
 *   · 检查放后台线程、启动后延迟 3 秒，不挡首屏；网络失败一律静默忽略
 *     （地铁、弱网下弹「检查更新失败」只会烦人）。
 *   · 只比 versionCode：versionName 是给人看的，可能回退或改格式。
 *   · 下载到 cacheDir 再交给系统安装器，装完系统会自己清理。
 *   · 「忽略此版本」记在 SharedPreferences，同版本不再打扰，下个版本继续提示。
 */
public final class Updater {

    /** 更新清单地址（与 download.example.com 的目录结构对应） */
    /** 留空 = 关闭自动更新；要开启就填自己的 latest.json 地址（格式见类注释） */
    private static final String MANIFEST_URL = "";
    private static final String PREF = "updater";
    private static final String KEY_SKIP_CODE = "skip_version_code";
    /** 启动后延迟多久检查，避开首屏渲染与登录校验 */
    private static final long STARTUP_DELAY_MS = 3000L;
    private static final int CONNECT_TIMEOUT_MS = 8000;
    private static final int READ_TIMEOUT_MS = 15000;

    private final Activity host;
    private final Handler ui = new Handler(Looper.getMainLooper());

    public Updater(Activity host) {
        this.host = host;
    }

    /** 启动时调用：延迟后台检查，任何异常都不外抛。 */
    public void checkOnStartup() {
        ui.postDelayed(new Runnable() {
            @Override
            public void run() {
                new Thread(new Runnable() {
                    @Override
                    public void run() {
                        try {
                            check();
                        } catch (Throwable ignored) {
                            // 自动检查失败不打扰用户
                        }
                    }
                }, "update-check").start();
            }
        }, STARTUP_DELAY_MS);
    }

    /**
     * 手动检查更新（设置页的按钮）。
     * 和启动检查的区别：结果一定要有反馈——已是最新、检查失败都要提示，
     * 否则用户点了没反应会以为按钮坏了。
     */
    public void checkManually() {
        new Thread(new Runnable() {
            @Override
            public void run() {
                try {
                    check(true);
                } catch (Throwable t) {
                    toastLater("检查更新失败：" + t.getMessage());
                }
            }
        }, "update-check-manual").start();
    }

    private void toastLater(final String text) {
        ui.post(new Runnable() {
            @Override
            public void run() {
                if (!host.isFinishing()) {
                    Toast.makeText(host, text, Toast.LENGTH_SHORT).show();
                }
            }
        });
    }

    private void check() throws Exception {
        check(false);
    }

    /** manual=true 时：已是最新要提示，跳过版本也要提示（手动点就该知道结果）。 */
    private void check(boolean manual) throws Exception {
        if (MANIFEST_URL.isEmpty()) {
            if (manual) {
                toastLater("本版本未配置更新清单（MANIFEST_URL 为空）");
            }
            return;
        }
        JSONObject root = new JSONObject(httpGet(MANIFEST_URL));
        JSONObject platforms = root.optJSONObject("platforms");
        JSONObject android = platforms == null ? null : platforms.optJSONObject("android");
        if (android == null) {
            return;
        }
        final int remoteCode = android.optInt("versionCode", 0);
        final String remoteName = android.optString("version", root.optString("version", ""));
        final String url = android.optString("url", "");
        final long size = android.optLong("size", 0);
        final String notes = root.optString("notes", "");

        int localCode = localVersionCode();
        if (remoteCode <= 0 || url.isEmpty() || remoteCode <= localCode) {
            if (manual) {
                toastLater("已是最新版本（" + localVersionName() + "）");
            }
            return;
        }
        if (host.getSharedPreferences(PREF, Context.MODE_PRIVATE)
                .getInt(KEY_SKIP_CODE, 0) == remoteCode) {
            if (manual) {
                toastLater("已是最新版本（" + localVersionName() + "）");
            }
            return;
        }

        ui.post(new Runnable() {
            @Override
            public void run() {
                showPrompt(remoteCode, remoteName, url, size, notes);
            }
        });
    }

    private void showPrompt(final int remoteCode, final String remoteName,
                            final String url, final long size, final String notes) {
        if (host.isFinishing()) {
            return;
        }
        StringBuilder msg = new StringBuilder();
        msg.append("版本 ").append(remoteName).append("（").append(humanSize(size)).append("）");
        if (!notes.isEmpty()) {
            msg.append("\n\n").append(notes);
        }
        // 注意：这里不能用 lambda。构建用 -bootclasspath android.jar，javac 在其上
        // 找不到 java.lang.invoke.LambdaMetafactory，lambda 会直接编译失败。
        new AlertDialog.Builder(host)
                .setTitle("发现新版本")
                .setMessage(msg.toString())
                .setPositiveButton("立即更新", new DialogInterface.OnClickListener() {
                    @Override
                    public void onClick(DialogInterface d, int w) {
                        download(url, size, remoteName);
                    }
                })
                .setNegativeButton("稍后", null)
                .setNeutralButton("忽略此版本", new DialogInterface.OnClickListener() {
                    @Override
                    public void onClick(DialogInterface d, int w) {
                        host.getSharedPreferences(PREF, Context.MODE_PRIVATE).edit()
                                .putInt(KEY_SKIP_CODE, remoteCode).apply();
                    }
                })
                .show();
    }

    /** 下载 -> 校验大小 -> 交给系统安装器。进度用不可取消的对话框，避免半截文件被安装。 */
    private void download(final String url, final long expectSize, final String versionName) {
        final ProgressDialog pd = new ProgressDialog(host);
        pd.setTitle("正在下载 " + versionName);
        pd.setProgressStyle(ProgressDialog.STYLE_HORIZONTAL);
        pd.setMax(expectSize > 0 ? (int) Math.min(expectSize, Integer.MAX_VALUE) : 100);
        pd.setCancelable(false);
        pd.show();

        new Thread(new Runnable() {
            @Override
            public void run() {
                File apk = null;
                String error = null;
                try {
                    File dir = new File(host.getCacheDir(), "update");
                    if (!dir.exists() && !dir.mkdirs()) {
                        throw new IllegalStateException("无法创建缓存目录");
                    }
                    // 每次都换个文件名，避免系统安装器缓存上一次的包
                    File tmp = new File(dir, "app-" + System.currentTimeMillis() + ".apk");
                    HttpURLConnection conn = open(url);
                    try {
                        long total = conn.getContentLength();
                        InputStream in = conn.getInputStream();
                        FileOutputStream out = new FileOutputStream(tmp);
                        byte[] buf = new byte[16 * 1024];
                        long done = 0;
                        int n;
                        while ((n = in.read(buf)) > 0) {
                            out.write(buf, 0, n);
                            done += n;
                            if (total > 0) {
                                updateProgress(pd, (int) Math.min(done, Integer.MAX_VALUE));
                            }
                        }
                        out.flush();
                        out.close();
                        in.close();
                        if (tmp.length() == 0) {
                            throw new IllegalStateException("下载内容为空");
                        }
                        if (expectSize > 0 && tmp.length() != expectSize) {
                            throw new IllegalStateException("下载不完整");
                        }
                        apk = tmp;
                    } finally {
                        conn.disconnect();
                    }
                } catch (Throwable e) {
                    error = e.getMessage() == null ? e.getClass().getSimpleName() : e.getMessage();
                }

                final File finalApk = apk;
                final String finalError = error;
                ui.post(new Runnable() {
                    @Override
                    public void run() {
                        pd.dismiss();
                        if (finalApk != null) {
                            install(finalApk);
                        } else {
                            Toast.makeText(host, "更新下载失败：" + finalError,
                                    Toast.LENGTH_LONG).show();
                        }
                    }
                });
            }
        }, "update-download").start();
    }

    private void updateProgress(final ProgressDialog pd, final int progress) {
        ui.post(new Runnable() {
            @Override
            public void run() {
                pd.setProgress(progress);
            }
        });
    }

    /**
     * 交给系统安装器。
     *
     * Android 8+ 需要「安装未知应用」授权：没授权时先跳到本应用的设置页，
     * 让用户手动打开——直接 startActivity 会因为缺少权限静默失败。
     */
    private void install(File apk) {
        if (Build.VERSION.SDK_INT >= 26 && !host.getPackageManager().canRequestPackageInstalls()) {
            Toast.makeText(host, "请允许本应用安装应用，然后重新点更新", Toast.LENGTH_LONG).show();
            try {
                Intent intent = new Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES,
                        Uri.parse("package:" + host.getPackageName()));
                host.startActivity(intent);
            } catch (Throwable ignored) {
            }
            return;
        }
        try {
            Uri uri = ApkProvider.uriFor(apk);
            Intent intent = new Intent(Intent.ACTION_VIEW);
            intent.setDataAndType(uri, "application/vnd.android.package-archive");
            intent.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION);
            intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
            host.startActivity(intent);
        } catch (Throwable e) {
            Toast.makeText(host, "无法启动安装器：" + e.getMessage(), Toast.LENGTH_LONG).show();
        }
    }

    private int localVersionCode() {
        try {
            PackageInfo info = host.getPackageManager().getPackageInfo(host.getPackageName(), 0);
            if (Build.VERSION.SDK_INT >= 28) {
                return (int) info.getLongVersionCode();
            }
            return info.versionCode;
        } catch (Throwable e) {
            return 0;
        }
    }

    private String localVersionName() {
        try {
            PackageInfo info = host.getPackageManager().getPackageInfo(host.getPackageName(), 0);
            return info.versionName == null ? "未知" : info.versionName;
        } catch (Throwable e) {
            return "未知";
        }
    }

    private static HttpURLConnection open(String url) throws Exception {
        HttpURLConnection conn = (HttpURLConnection) new URL(url).openConnection();
        conn.setConnectTimeout(CONNECT_TIMEOUT_MS);
        conn.setReadTimeout(READ_TIMEOUT_MS);
        conn.setInstanceFollowRedirects(true);
        conn.setRequestProperty("Accept", "application/json, */*");
        conn.setRequestProperty("User-Agent", "chat-demo-android/updater");
        int code = conn.getResponseCode();
        if (code < 200 || code >= 300) {
            conn.disconnect();
            throw new IllegalStateException("HTTP " + code);
        }
        return conn;
    }

    private static String httpGet(String url) throws Exception {
        HttpURLConnection conn = open(url);
        try {
            BufferedReader r = new BufferedReader(
                    new InputStreamReader(conn.getInputStream(), StandardCharsets.UTF_8));
            StringBuilder sb = new StringBuilder();
            String line;
            while ((line = r.readLine()) != null) {
                sb.append(line);
            }
            r.close();
            return sb.toString();
        } finally {
            conn.disconnect();
        }
    }

    private static String humanSize(long n) {
        if (n <= 0) return "未知大小";
        if (n < 1024 * 1024) return (n / 1024) + " KB";
        return String.format(java.util.Locale.US, "%.1f MB", n / 1024.0 / 1024.0);
    }
}
