package ink.sipc.nagi.data

import android.content.Context
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow

/** 外观：跟随系统、浅色、深色。 */
enum class ThemeMode { System, Light, Dark }

/** 界面语言：跟随系统、中文、English。[tag] 是 BCP 47 语言标签，跟随系统时为 null。 */
enum class AppLanguage(val tag: String?) { System(null), Chinese("zh-CN"), English("en") }

/** App 自己的设置（见 docs/android.md#本机设置），只存在本机。 */
class AppPrefs(context: Context) {
    private val prefs = context.applicationContext.getSharedPreferences("app", Context.MODE_PRIVATE)

    private val _theme = MutableStateFlow(enumOr(prefs.getString(KEY_THEME, null), ThemeMode.System))
    val theme: StateFlow<ThemeMode> = _theme

    private val _language = MutableStateFlow(enumOr(prefs.getString(KEY_LANGUAGE, null), AppLanguage.System))
    val language: StateFlow<AppLanguage> = _language

    fun setTheme(mode: ThemeMode) {
        prefs.edit().putString(KEY_THEME, mode.name).apply()
        _theme.value = mode
    }

    fun setLanguage(language: AppLanguage) {
        // 切换语言后界面马上重建，要先写进去，重建时读到的才是新的。
        prefs.edit().putString(KEY_LANGUAGE, language.name).commit()
        _language.value = language
    }

    companion object {
        private const val KEY_THEME = "theme"
        private const val KEY_LANGUAGE = "language"

        /** Activity 创建前（attachBaseContext）读语言用，这时 AppGraph 可能还没有。 */
        fun language(context: Context): AppLanguage =
            enumOr(context.getSharedPreferences("app", Context.MODE_PRIVATE).getString(KEY_LANGUAGE, null), AppLanguage.System)

        private inline fun <reified T : Enum<T>> enumOr(name: String?, default: T): T =
            enumValues<T>().firstOrNull { it.name == name } ?: default
    }
}
