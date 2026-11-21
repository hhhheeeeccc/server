package com.muhammil.videos;

import com.grack.nanojson.JsonObject;
import org.schabi.newpipe.extractor.localization.ContentCountry;
import org.schabi.newpipe.extractor.localization.Localization;
import org.schabi.newpipe.extractor.services.youtube.YoutubeStreamHelper;


/**
 * جسر جافا لاستدعاء YoutubeStreamHelper بتمرير null لمعامل playerTimestamp.
 * Kotlin يرفض null للمعامل المعلَّم @NonNull بينما الطريقة نفسها تقبله وقت التشغيل
 * (وهكذا كان يعمل DownLite 2.3).
 */
public final class YoutubeHelperBridge {

    private YoutubeHelperBridge() {
    }

    public static JsonObject visionOsPlayerResponse(String videoId) throws Exception {
        return YoutubeStreamHelper.getVisionOsPlayerResponse(
                new ContentCountry("US"), new Localization("en", "US"), videoId, null);
    }
}
