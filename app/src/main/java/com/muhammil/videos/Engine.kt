package com.muhammil.videos

import android.content.Context
import android.util.Log
import com.yausername.ffmpeg.FFmpeg
import com.yausername.youtubedl_android.YoutubeDL
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * حالة محرك التنزيل (yt-dlp).
 *
 * هذا الملف هو إصلاح المشكلة الأولى: "يتم الاتصال بمحرك التنزيلات ثم يخرج ولا يحدث شيء".
 * السبب كان أن أي خطأ أثناء التهيئة كان يُبتلع بصمت. الآن:
 *  - التهيئة تجري بالكامل في خيط خلفي ولا تعطل واجهة المستخدم.
 *  - كل فشل يُسجَّل ويظهر للمستخدم بنص عربي واضح مع زر إعادة المحاولة.
 *  - لا يوجد أي انهيار صامت؛ الحالة دائمًا مرئية عبر [state].
 */
object Engine {

    private const val TAG = "Engine"

    sealed class State {
        data object NotStarted : State()
        data object Initializing : State()
        data object Ready : State()
        data class Failed(val reason: String) : State()
    }

    private val _state = MutableStateFlow<State>(State.NotStarted)
    val state: StateFlow<State> = _state.asStateFlow()

    @Synchronized
    fun start(context: Context) {
        val current = _state.value
        if (current is State.Initializing || current is State.Ready) return
        _state.value = State.Initializing
        val appContext = context.applicationContext
        Thread {
            var lastError: Exception? = null
            var attempt = 0
            // ثلاث محاولات مع مهلة تصاعدية بدل الفشل الصامت
            while (attempt < 3 && _state.value !is State.Ready) {
                try {
                    YoutubeDL.getInstance().init(appContext)
                    FFmpeg.init(appContext)
                    Log.i(TAG, "engine ready")
                    _state.value = State.Ready
                } catch (e: Exception) {
                    lastError = e
                    Log.e(TAG, "فشل تهيئة المحرك (المحاولة ${attempt + 1}/3)", e)
                    Thread.sleep(2000L * (attempt + 1))
                }
                attempt++
            }
            if (_state.value !is State.Ready) {
                _state.value = State.Failed(lastError?.message ?: "unknown error")
            }
        }.start()
    }

    fun retry(context: Context) {
        _state.value = State.NotStarted
        start(context)
    }
}
