package com.nuvio.app.core.ui

import androidx.compose.foundation.Image
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.layout.ContentScale
import coil3.compose.AsyncImage

/**
 * Remote image for iOS: collection artwork, profile avatars, badges, posters.
 *
 * Animated sources (GIF and animated WebP) go through Skia (see
 * [rememberAnimatedFrame]); everything else goes through Coil. There is no
 * UIKitView interop involved.
 *
 * The measured size is passed down so frames are decoded at the size they are
 * actually drawn at — a 16 dp badge and a 160 dp poster should not cost the
 * same. Call sites of equal size share one decoded animation.
 */
@Composable
internal actual fun NuvioAsyncImage(
    imageUrl: String,
    contentDescription: String,
    modifier: Modifier,
    contentScale: ContentScale,
    animateIfPossible: Boolean,
) {
    if (!animateIfPossible || !imageUrl.looksAnimated()) {
        AsyncImage(
            model = imageUrl,
            contentDescription = contentDescription,
            modifier = modifier,
            contentScale = contentScale,
        )
        return
    }

    BoxWithConstraints(modifier = modifier) {
        // Constraints are in pixels. Unbounded axes fall back to a sane default
        // rather than decoding at source resolution.
        val targetWidth = constraints.maxWidth
            .takeIf { it in 1..MaxAnimationTargetEdgePx }
            ?: DefaultAnimationTargetEdgePx
        val targetHeight = constraints.maxHeight
            .takeIf { it in 1..MaxAnimationTargetEdgePx }
            ?: DefaultAnimationTargetEdgePx

        val frame = rememberAnimatedFrame(imageUrl, targetWidth, targetHeight)

        if (frame == null) {
            // Loading, not animated, or over budget — all render statically.
            AsyncImage(
                model = imageUrl,
                contentDescription = contentDescription,
                modifier = Modifier.matchParentSize(),
                contentScale = contentScale,
            )
        } else {
            Image(
                bitmap = frame,
                contentDescription = contentDescription,
                modifier = Modifier.matchParentSize(),
                contentScale = contentScale,
            )
        }
    }
}

/**
 * Extension-based detection.
 *
 * Known gap: CDN URLs without an extension are never detected. Sniffing magic
 * bytes after download would cover those, but that is a separate change.
 */
private fun String.looksAnimated(): Boolean {
    val cleanUrl = substringBefore('?').substringBefore('#')
    return cleanUrl.endsWith(".gif", ignoreCase = true) ||
        cleanUrl.endsWith(".webp", ignoreCase = true)
}
