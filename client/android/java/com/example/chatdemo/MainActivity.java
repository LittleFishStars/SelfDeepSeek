package com.example.chatdemo;

import android.app.Activity;
import android.content.Intent;
import android.provider.MediaStore;
import android.content.pm.ActivityInfo;
import android.graphics.Color;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.view.View;
import android.view.ViewGroup;
import android.view.Window;
import android.content.ActivityNotFoundException;
import android.webkit.JavascriptInterface;
import android.webkit.PermissionRequest;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebChromeClient.FileChooserParams;
import android.webkit.WebView;
import android.webkit.WebViewClient;

/**
 * 一个把 DeepSeek chat-demo（纯静态网页）装进 WebView 的壳。
 *
 * 静态文件通过内置的 127.0.0.1 HTTP 服务提供，而不是 file://：
 * file:// 下 localStorage / IndexedDB 会被 WebView 限制，聊天记录存不下来。
 */
public class MainActivity extends Activity {

    private WebView webView;
    private MiniServer server;
    private Updater updater;
    /** 网页里 <input type=file> 的回调，必须在 onShowFileChooser 里接住，否则「拍照/相册/文件」点了没反应 */
    private ValueCallback<Uri[]> pendingFileCallback;
    private static final int REQ_PICK_FILE = 1001;
    private static final int REQ_CAPTURE = 1002;
    /** 拍照输出地址，拍完要回传给网页 */
    private Uri pendingCaptureUri;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);

        // 手机竖屏为主，但允许横屏（横屏时布局由网页自己适配）
        setRequestedOrientation(ActivityInfo.SCREEN_ORIENTATION_UNSPECIFIED);

        Window window = getWindow();
        window.setStatusBarColor(Color.WHITE);
        if (Build.VERSION.SDK_INT >= 23) {
            View decor = window.getDecorView();
            decor.setSystemUiVisibility(decor.getSystemUiVisibility()
                    | View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR);
        }

        server = new MiniServer(this);
        int port = server.start();
        if (port <= 0) {
            // 端口起不来时退回 file://，至少还能打开页面
            port = 0;
        }

        webView = new WebView(this);
        setContentView(webView, new ViewGroup.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));

        WebSettings s = webView.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setDatabaseEnabled(true);
        s.setMediaPlaybackRequiresUserGesture(false);
        s.setSupportZoom(false);
        s.setBuiltInZoomControls(false);
        s.setAllowFileAccess(false);
        s.setAllowContentAccess(true);
        s.setLoadWithOverviewMode(true);
        s.setUseWideViewPort(true);
        if (Build.VERSION.SDK_INT >= 26) {
            s.setSafeBrowsingEnabled(false);
        }

        webView.setWebChromeClient(new WebChromeClient() {
            @Override
            public boolean onShowFileChooser(WebView view, ValueCallback<Uri[]> callback,
                                             FileChooserParams params) {
                if (pendingFileCallback != null) {
                    pendingFileCallback.onReceiveValue(null);
                }
                pendingFileCallback = callback;
                // accept 里带 capture 的属性：网页的「拍照」按钮传了 capture=environment，
                // 这里直接调系统相机，否则 WebView 只会弹相册
                if (params.isCaptureEnabled()) {
                    try {
                        String name = "shot-" + System.currentTimeMillis() + ".jpg";
                        pendingCaptureUri = ShotProvider.uriFor(name);
                        Intent capture = new Intent(MediaStore.ACTION_IMAGE_CAPTURE);
                        capture.putExtra(MediaStore.EXTRA_OUTPUT, pendingCaptureUri);
                        capture.addFlags(Intent.FLAG_GRANT_WRITE_URI_PERMISSION);
                        startActivityForResult(capture, REQ_CAPTURE);
                        return true;
                    } catch (ActivityNotFoundException e) {
                        // 没相机就退回普通选择器
                        pendingCaptureUri = null;
                    }
                }
                try {
                    startActivityForResult(params.createIntent(), REQ_PICK_FILE);
                } catch (ActivityNotFoundException e) {
                    pendingFileCallback = null;
                    return false;
                }
                return true;
            }

            @Override
            public void onPermissionRequest(final PermissionRequest request) {
                // 语音输入等需要麦克风权限时，直接授予网页
                runOnUiThread(new Runnable() {
                    @Override
                    public void run() {
                        request.grant(request.getResources());
                    }
                });
            }
        });

        webView.setWebViewClient(new WebViewClient() {
            @Override
            public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest request) {
                Uri uri = request.getUrl();
                String scheme = uri == null ? "" : uri.getScheme();
                if ("http".equals(scheme) || "https".equals(scheme)) {
                    return false; // 站内跳转交给 WebView
                }
                try {
                    startActivity(new Intent(Intent.ACTION_VIEW, uri));
                } catch (Throwable ignored) {
                }
                return true;
            }
        });

        // 直接进对话页：页面内的登录守卫会判断有没有令牌，没令牌才跳登录页，
        // 已有令牌则静默校验后继续用（不再每次启动都停在登录页）。
        String url = port > 0
                ? "http://127.0.0.1:" + port + "/index.html"
                : "file:///android_asset/index.html";
        // 自动更新：延迟几秒在后台查清单，不参与首屏渲染；失败静默
        updater = new Updater(this);
        updater.checkOnStartup();

        // 设置页的「检查更新」按钮通过这个桥调用原生检查：网页侧拿不到 APK 的
        // versionCode，也没法自己拉安装器，只能交给 Updater。
        // 必须在 loadUrl 之前注册：页面脚本一执行就要读 window.chatUpdate，
        // 加载完再注册的话首屏拿不到（要刷新一次才出现）。
        webView.addJavascriptInterface(new UpdateBridge(), "chatUpdate");

        webView.loadUrl(url);
    }

    @Override
    protected void onActivityResult(int requestCode, int resultCode, Intent data) {
        if (requestCode == REQ_CAPTURE) {
            if (pendingFileCallback != null) {
                pendingFileCallback.onReceiveValue(
                        resultCode == RESULT_OK && pendingCaptureUri != null
                                ? new Uri[]{pendingCaptureUri}
                                : null);
                pendingFileCallback = null;
            }
            pendingCaptureUri = null;
            return;
        }
        if (requestCode == REQ_PICK_FILE) {
            if (pendingFileCallback != null) {
                Uri[] results = null;
                if (resultCode == RESULT_OK && data != null) {
                    if (data.getClipData() != null) {
                        int count = data.getClipData().getItemCount();
                        results = new Uri[count];
                        for (int i = 0; i < count; i++) {
                            results[i] = data.getClipData().getItemAt(i).getUri();
                        }
                    } else if (data.getData() != null) {
                        results = new Uri[]{data.getData()};
                    }
                }
                pendingFileCallback.onReceiveValue(results);
                pendingFileCallback = null;
            }
            return;
        }
        super.onActivityResult(requestCode, resultCode, data);
    }

    @Override
    public void onBackPressed() {
        if (webView != null && webView.canGoBack()) {
            webView.goBack();
            return;
        }
        super.onBackPressed();
    }

    @Override
    protected void onDestroy() {
        if (server != null) {
            server.stop();
        }
        if (webView != null) {
            webView.destroy();
        }
        super.onDestroy();
    }

    /**
     * 暴露给网页的更新入口。
     * 注意：@JavascriptInterface 必需，否则 Android 4.2+ 会静默拒绝调用。
     */
    public class UpdateBridge {
        @JavascriptInterface
        public boolean available() {
            return true;
        }

        @JavascriptInterface
        public String versionName() {
            try {
                return getPackageManager().getPackageInfo(getPackageName(), 0).versionName;
            } catch (Throwable e) {
                return "";
            }
        }

        @JavascriptInterface
        public void check() {
            runOnUiThread(new Runnable() {
                @Override
                public void run() {
                    updater.checkManually();
                }
            });
        }
    }
}
