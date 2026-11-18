plugins {
    alias(libs.plugins.android.application) apply false
    alias(libs.plugins.android.library) apply false
    alias(libs.plugins.kotlin.android) apply false
    alias(libs.plugins.kotlin.compose) apply false
}

// connect-kotlin-okhttp 0.10.0 依赖 OkHttp 5.5.0，它要求 compileSdk 37，而 AGP 8.13 最高支持 36，
// release 构建的 AAR 元数据检查会失败。固定用 libs.versions.toml 里的 OkHttp 版本，升级 AGP 以后再去掉。
val okhttpVersion = libs.versions.okhttp.get()
subprojects {
    configurations.configureEach {
        resolutionStrategy.eachDependency {
            if (requested.group == "com.squareup.okhttp3" && requested.name.startsWith("okhttp")) {
                useVersion(okhttpVersion)
                because("OkHttp 5.5.0 要求 compileSdk 37")
            }
        }
    }
}
