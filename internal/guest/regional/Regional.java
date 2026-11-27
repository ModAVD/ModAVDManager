// Copyright 2026 jinghero. All rights reserved.
// SPDX-License-Identifier: AGPL-3.0-or-later
package org.modavd.regional;

import java.io.File;
import java.io.FileOutputStream;
import java.lang.reflect.Method;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.Locale;
import java.util.TimeZone;

// app_process runs this as uid 0 on the supported userdebug image. No APK, service, or reboot is installed.
public final class Regional {
    private static final File APPLIED = new File("/data/local/tmp/modavd-language-applied");

    public static void main(String[] args) throws Exception {
        Class<?> manager = Class.forName("android.app.IActivityManager");
        Class<?> configType = Class.forName("android.content.res.Configuration");
        Class<?> listType = Class.forName("android.os.LocaleList");
        Object service = Class.forName("android.app.ActivityManager").getMethod("getService").invoke(null);
        Method getConfig = manager.getMethod("getConfiguration");
        Method getLocales = configType.getMethod("getLocales");
        Method tags = listType.getMethod("toLanguageTags");

        if (args.length == 2) {
            String id = args[0];
            if (!id.matches("[a-f0-9]{32}")) throw new IllegalArgumentException("invalid request id");
            Locale locale = new Locale.Builder().setLanguageTag(args[1]).build();
            if (locale.getLanguage().isEmpty() || locale.getLanguage().equals("und")) {
                throw new IllegalArgumentException("invalid language");
            }
            String previous = APPLIED.isFile()
                    ? new String(Files.readAllBytes(APPLIED.toPath()), StandardCharsets.US_ASCII) : "";
            if (!id.equals(previous)) {
                // Android's locale picker updates persistent Configuration. Writing system_locales alone leaves
                // the current Configuration unchanged. Resolve methods at runtime instead of pinning Binder IDs.
                Object config = configType.getConstructor().newInstance();
                Object list = listType.getMethod("forLanguageTags", String.class).invoke(null, args[1]);
                configType.getMethod("setLocales", listType).invoke(config, list);
                configType.getField("userSetLocale").setBoolean(config, true);
                manager.getMethod("updatePersistentConfiguration", configType).invoke(service, config);
                String actual = (String) tags.invoke(getLocales.invoke(getConfig.invoke(service)));
                if (!actual.equals(args[1])) throw new IllegalStateException("language readback: " + actual);
                File temporary = new File(APPLIED.getPath() + ".tmp");
                try (FileOutputStream out = new FileOutputStream(temporary)) {
                    out.write(id.getBytes(StandardCharsets.US_ASCII));
                    out.getFD().sync();
                }
                if (!temporary.renameTo(APPLIED)) throw new IllegalStateException("language receipt rename failed");
            }
            // A repeated request acknowledges its receipt without undoing a subsequent change in Android.
            System.out.println("applied=" + id);
        } else if (args.length != 0) {
            throw new IllegalArgumentException("expected no arguments, or request id and language");
        }
        System.out.println("language=" + tags.invoke(getLocales.invoke(getConfig.invoke(service))));
        System.out.println("time_zone=" + TimeZone.getDefault().getID());
    }
}
