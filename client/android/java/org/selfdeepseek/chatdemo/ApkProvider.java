package org.selfdeepseek.chatdemo;

import android.content.ContentProvider;
import android.content.ContentValues;
import android.content.Context;
import android.database.Cursor;
import android.database.MatrixCursor;
import android.net.Uri;
import android.os.ParcelFileDescriptor;
import android.provider.OpenableColumns;

import java.io.File;
import java.io.FileNotFoundException;

/**
 * 极简 FileProvider：把下载好的 APK 以 content:// 交给系统安装器。
 *
 * 直接给安装器传 file:// 会在 Android 7+ 抛 FileUriExposedException；
 * 完整版 androidx FileProvider 会引入依赖，这里按 ShotProvider 的写法再来一份。
 *
 * 只暴露 cacheDir/update 下的文件，且路径必须落在该目录内（防目录穿越）。
 */
public class ApkProvider extends ContentProvider {

    public static final String AUTHORITY = "org.selfdeepseek.chatdemo.apks";

    static File dir(Context context) {
        File d = new File(context.getCacheDir(), "update");
        if (!d.exists()) {
            //noinspection ResultOfMethodCallIgnored
            d.mkdirs();
        }
        return d;
    }

    /** 用文件名（不带路径）换取 content:// 地址 */
    public static Uri uriFor(File apk) {
        return Uri.parse("content://" + AUTHORITY + "/" + apk.getName());
    }

    @Override
    public boolean onCreate() {
        return true;
    }

    private File fileFor(Uri uri) throws FileNotFoundException {
        String name = uri.getLastPathSegment();
        if (name == null || name.isEmpty()
                || name.contains("/") || name.contains("..")) {
            throw new FileNotFoundException("bad name");
        }
        File root = dir(getContext());
        File f = new File(root, name);
        try {
            // 规范化后再校验，防止 ../ 之类的写法绕出缓存目录
            if (!f.getCanonicalPath().startsWith(root.getCanonicalPath() + File.separator)) {
                throw new FileNotFoundException("out of root");
            }
        } catch (java.io.IOException e) {
            throw new FileNotFoundException(e.getMessage());
        }
        return f;
    }

    @Override
    public ParcelFileDescriptor openFile(Uri uri, String mode) throws FileNotFoundException {
        return ParcelFileDescriptor.open(fileFor(uri), ParcelFileDescriptor.MODE_READ_ONLY);
    }

    @Override
    public Cursor query(Uri uri, String[] projection, String selection, String[] args, String sort) {
        File f;
        try {
            f = fileFor(uri);
        } catch (FileNotFoundException e) {
            return null;
        }
        String[] cols = projection != null ? projection
                : new String[]{OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE};
        MatrixCursor c = new MatrixCursor(cols, 1);
        Object[] row = new Object[cols.length];
        for (int i = 0; i < cols.length; i++) {
            if (OpenableColumns.DISPLAY_NAME.equals(cols[i])) {
                row[i] = f.getName();
            } else if (OpenableColumns.SIZE.equals(cols[i])) {
                row[i] = f.length();
            } else {
                row[i] = null;
            }
        }
        c.addRow(row);
        return c;
    }

    @Override
    public String getType(Uri uri) {
        return "application/vnd.android.package-archive";
    }

    @Override
    public Uri insert(Uri uri, ContentValues values) {
        return null;
    }

    @Override
    public int delete(Uri uri, String selection, String[] args) {
        return 0;
    }

    @Override
    public int update(Uri uri, ContentValues values, String selection, String[] args) {
        return 0;
    }
}
