package ink.sipc.nagi.settings

import android.app.Activity
import android.app.LocaleManager
import android.content.Context
import android.content.res.Configuration
import android.os.Build
import android.os.LocaleList
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import ink.sipc.nagi.data.AppLanguage
import ink.sipc.nagi.data.AppPrefs
import ink.sipc.nagi.data.PanelSession
import ink.sipc.nagi.proto.v1.AppInfoResponse
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import java.util.Locale

/** 设置页：账号（名称、角色、头像）和面板版本来自 AppService.Info，打开设置页时取一次。 */
class SettingsViewModel(private val session: PanelSession) : ViewModel() {
    private val _info = MutableStateFlow<AppInfoResponse?>(null)
    val info: StateFlow<AppInfoResponse?> = _info

    /** 面板名称留空时用的默认名称（主机名），只有管理员能查（SettingsService.GetSettings）。 */
    private val _defaultName = MutableStateFlow<String?>(null)
    val defaultName: StateFlow<String?> = _defaultName

    init {
        refresh()
    }

    fun refresh() {
        viewModelScope.launch {
            runCatching { session.info() }.onSuccess {
                _info.value = it
                if (it.role == "admin") runCatching { session.panelSettings() }.onSuccess { s -> _defaultName.value = s.defaultName }
            }
        }
    }
}

/**
 * App 内切换语言（见 docs/android.md#本机设置）。Android 13 起交给系统的「应用语言」（系统设置里也能改，切换后系统重建界面）；
 * 更早的版本自己在 Activity 创建时换掉 Configuration 里的语言（[wrap]），切换后重建 Activity。
 */
object AppLocales {
    fun apply(activity: Activity, prefs: AppPrefs, language: AppLanguage) {
        prefs.setLanguage(language)
        if (Build.VERSION.SDK_INT >= 33) {
            activity.getSystemService(LocaleManager::class.java).applicationLocales =
                language.tag?.let { LocaleList.forLanguageTags(it) } ?: LocaleList.getEmptyLocaleList()
        } else {
            activity.recreate()
        }
    }

    /** 当前的选择。Android 13 起以系统的「应用语言」为准（用户可能在系统设置里改过）。 */
    fun current(context: Context, prefs: AppPrefs): AppLanguage {
        if (Build.VERSION.SDK_INT < 33) return prefs.language.value
        val tags = context.getSystemService(LocaleManager::class.java).applicationLocales.toLanguageTags()
        return when {
            tags.isEmpty() -> AppLanguage.System
            tags.startsWith("zh") -> AppLanguage.Chinese
            tags.startsWith("en") -> AppLanguage.English
            else -> AppLanguage.System
        }
    }

    /** Android 13 以前在 Activity 的 attachBaseContext 里调用：按本机设置换掉语言。 */
    fun wrap(base: Context): Context {
        if (Build.VERSION.SDK_INT >= 33) return base
        val tag = AppPrefs.language(base).tag ?: return base
        val locale = Locale.forLanguageTag(tag)
        Locale.setDefault(locale)
        val config = Configuration(base.resources.configuration).apply { setLocale(locale) }
        return base.createConfigurationContext(config)
    }
}
