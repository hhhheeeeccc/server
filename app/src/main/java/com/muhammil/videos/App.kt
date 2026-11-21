package com.muhammil.videos

import android.app.Application

class App : Application() {
    override fun onCreate() {
        super.onCreate()
        // تجهيز محرك التنزيل مباشرة عند تشغيل التطبيق (في الخلفية)
        Engine.start(this)
    }
}
