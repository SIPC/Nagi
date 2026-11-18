plugins {
    alias(libs.plugins.android.library)
    alias(libs.plugins.kotlin.android)

}

android {
    namespace = "ink.sipc.nagi.network"
    compileSdk = 36
    defaultConfig { minSdk = 28 }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

}

kotlin { compilerOptions { jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17) } }

dependencies {
    api(project(":core:proto"))
    api(libs.okhttp)
    // connect-kotlin 0.10 要 OkHttp 5.5，它的 Android 版要求 compileSdk 37；5.4 只要 36，接口相同。
    constraints {
        api("com.squareup.okhttp3:okhttp:5.4.0") { version { strictly("5.4.0") } }
        api("com.squareup.okhttp3:okhttp-jvm:5.4.0") { version { strictly("5.4.0") } }
        api("com.squareup.okhttp3:okhttp-android:5.4.0") { version { strictly("5.4.0") } }
    }
    implementation(project(":core:security"))
    implementation(libs.kotlinx.coroutines.android)
    testImplementation(libs.junit)
}
