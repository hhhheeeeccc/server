package com.muhammil.videos

import java.io.File

/** خطأ برسالة جاهزة من الموارد العربية. */
class AppException(val resId: Int) : Exception()

/** ناتج التنزيل قبل حفظه في مجلد التنزيلات. */
class EngineOutput(val file: File, val mime: String, val title: String)

/** يرميها المحرك إذا كان الرابط خارج نطاقه (فيُعاد التوجيه للاستراتيجيات اليدوية). */
class EngineUnsupportedException(message: String) : Exception(message)
