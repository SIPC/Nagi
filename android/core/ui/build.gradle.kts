plugins {
    alias(libs.plugins.android.library)
    alias(libs.plugins.kotlin.android)
    alias(libs.plugins.kotlin.compose)
}

android {
    namespace = "ink.sipc.nagi.ui"
    compileSdk = 36
    defaultConfig { minSdk = 28 }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    buildFeatures { compose = true }
}

kotlin { compilerOptions { jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17) } }

dependencies {
    implementation(platform(libs.compose.bom))
    implementation(libs.compose.ui)
    implementation(libs.compose.foundation)
    implementation(libs.compose.animation)
    implementation(libs.compose.material3)
    implementation(libs.compose.ui.tooling.preview)
    debugImplementation(libs.compose.ui.tooling)
    implementation(libs.androidx.core.ktx)
    implementation(libs.kotlinx.coroutines.android)
    // 终端：Termux 的解析和绘制（Apache 2.0），连接由 App 自己接（见 Terminal.kt）。
    api(libs.termux.terminal.emulator)
    implementation(libs.termux.terminal.view)
    // 编辑器：Sora Editor（LGPL-2.1，以库的方式引用），语法着色用 TextMate 语法（assets/textmate，取自 VS Code，MIT）。
    implementation(libs.sora.editor)
    implementation(libs.sora.textmate)
    testImplementation(libs.junit)
}
