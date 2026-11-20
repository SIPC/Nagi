pluginManagement {
    repositories {
        google()
        mavenCentral()
        gradlePluginPortal()
    }
}
dependencyResolutionManagement {
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        google()
        mavenCentral()
        // Termux 的终端库只发布在 JitPack（docs/android.md#日志终端和编辑器）：只从这里取它这一组，别的依赖不走 JitPack。
        maven("https://jitpack.io") {
            content { includeGroup("com.github.termux.termux-app") }
        }
    }
}

rootProject.name = "nagi"

include(":app")
include(":core:proto", ":core:network", ":core:security", ":core:data", ":core:ui")
include(":feature:home", ":feature:pairing", ":feature:settings", ":feature:nodes")
