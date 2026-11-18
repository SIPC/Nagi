plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.android)
    alias(libs.plugins.kotlin.compose)
}

// 版本号：发布时由 CI 传入 -PnagiVersion=<标签名去掉 v>，和面板同一个版本号；本地编译是 dev。
// versionCode 由版本号算出：主版本 × 1000000 + 次版本 × 1000 + 修订号，一版比一版大。
val nagiVersion = (findProperty("nagiVersion") as String?)?.removePrefix("v") ?: "dev"
val nagiVersionCode = Regex("""^(\d+)\.(\d+)\.(\d+)""").find(nagiVersion)?.destructured
    ?.let { (major, minor, patch) -> major.toInt() * 1_000_000 + minor.toInt() * 1_000 + patch.toInt() } ?: 1

// 发布签名：CI 从仓库的 Secrets 解出密钥库，用下面四个环境变量传入（见 docs/development.md#发布）。
// 没有这些变量时不签名，release 构建产出未签名的 APK，CI 不上传它。
val releaseKeystore = System.getenv("NAGI_KEYSTORE_FILE")?.takeIf { it.isNotBlank() }

android {
    namespace = "ink.sipc.nagi"
    compileSdk = 36
    defaultConfig {
        // 包名要和面板 auth.AppPackage 一致：添加手机时面板核对密钥证明里的包名。
        applicationId = "ink.sipc.nagi"
        minSdk = 28
        targetSdk = 36
        versionCode = nagiVersionCode
        versionName = nagiVersion
    }
    signingConfigs {
        if (releaseKeystore != null) {
            create("release") {
                storeFile = file(releaseKeystore)
                storePassword = System.getenv("NAGI_KEYSTORE_PASSWORD")
                keyAlias = System.getenv("NAGI_KEY_ALIAS")
                keyPassword = System.getenv("NAGI_KEY_PASSWORD")
            }
        }
    }
    buildTypes {
        release {
            // 发布包开启 R8 的代码裁剪和混淆，再去掉没用到的资源，减小安装包。
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            if (releaseKeystore != null) signingConfig = signingConfigs.getByName("release")
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    buildFeatures {
        compose = true
        buildConfig = true
    }
}

kotlin { compilerOptions { jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17) } }

dependencies {
    implementation(project(":core:ui"))
    implementation(project(":core:data"))
    implementation(project(":feature:home"))
    implementation(project(":feature:pairing"))
    implementation(project(":feature:settings"))
    implementation(platform(libs.compose.bom))
    implementation(libs.compose.ui)
    implementation(libs.compose.foundation)
    implementation(libs.compose.animation)
    implementation(libs.compose.material3)
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.activity.compose)
    implementation(libs.androidx.fragment)
    implementation(libs.androidx.lifecycle.runtime.compose)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.androidx.lifecycle.process)
    implementation(libs.androidx.navigation.compose)
}
