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
    }
}

rootProject.name = "nagi"

include(":app")
include(":core:proto", ":core:network", ":core:security", ":core:data", ":core:ui")
include(":feature:home", ":feature:pairing")
