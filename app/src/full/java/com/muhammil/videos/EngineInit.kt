package com.muhammil.videos

import android.content.Context
import com.yausername.ffmpeg.FFmpeg
import com.yausername.youtubedl_android.YoutubeDL

/** تهيئة الطبعة الكاملة: استخراج مكتبات yt-dlp وFFmpeg الجاهزة للعمل. */
fun initEngine(context: Context) {
    YoutubeDL.getInstance().init(context)
    FFmpeg.init(context)
}
