import type { Metadata, Viewport } from "next";
import Script from "next/script";
import { ThemeProvider } from "@/components/theme-provider";
import { Toaster } from "@multica/ui/components/ui/sonner";
import { WebProviders } from "@/components/web-providers";
import { getRequestLocale } from "@/lib/request-locale";
import { HTML_LANG } from "@/lib/html-lang";
import { SITE_TITLE, TITLE_TEMPLATE } from "@/platform/document-title";
import {
  resolveBrowserApiBaseUrl,
  resolveBrowserWsUrl,
  resolveDevRemoteApiUrl,
} from "@/config/runtime-urls";
import { resolveGoogleAnalyticsId } from "@/config/google-analytics";
import { GoogleAnalytics } from "@/components/google-analytics";

// Fonts ship as npm packages and are bundled at build time, so `next build`
// needs no network access (next/font/google downloads from the Google Fonts
// CDN during the build). Same packages and files as the desktop renderer
// (apps/desktop/src/renderer/src/main.tsx); keep both lists in sync.
//
// ./globals.css maps each family onto the variable the stacks use
// (`--font-inter`, `--font-geist-mono`, `--font-jetbrains-mono`,
// `--font-source-serif`) and composes `--font-sans` with the per-locale CJK
// chain there, so it can be overridden per `<html lang>` and stays CSP-safe.
//
// Every face declares all subsets with `unicode-range`; the browser fetches a
// file only once a rendered character needs it, so the optional JetBrains Mono
// code font and the editorial serif cost nothing on routes that do not use them.
//
// Inter: the `opsz` files add the optical-size axis on top of weight, so 14px
// and below keep the open text cut while headings get Inter Display's tighter
// spacing (`font-optical-sizing: auto` is the CSS default). Italic is its own
// file; without it semantic italic labels, markdown <em> and blockquotes render
// as browser-synthesized oblique.
import "@fontsource-variable/inter/opsz.css";
import "@fontsource-variable/inter/opsz-italic.css";
// Mono has no explicit CJK fallback: CJK chars in code blocks cannot align to a
// mono grid anyway, so the browser default fallback handles the rare mixed case.
import "@fontsource-variable/geist-mono";
// Optional code font from the Appearance settings.
import "@fontsource-variable/jetbrains-mono";
// Editorial serif for onboarding headlines and the Appearance "Serif" font,
// with italic for h1 em accents.
import "@fontsource-variable/source-serif-4";
import "@fontsource-variable/source-serif-4/wght-italic.css";
import "./globals.css";

export const viewport: Viewport = {
  width: "device-width",
  initialScale: 1,
  // Draw under the camera, rounded corners and home indicator instead of
  // letterboxing the page inside the safe area. Without it iOS reports every
  // env(safe-area-inset-*) as 0. globals.css keeps content clear of those
  // regions.
  viewportFit: "cover",
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#ffffff" },
    { media: "(prefers-color-scheme: dark)", color: "#05070b" },
  ],
};

export const metadata: Metadata = {
  metadataBase: new URL("https://www.multica.ai"),
  title: {
    default: SITE_TITLE,
    template: TITLE_TEMPLATE,
  },
  description:
    "Source-available platform that turns coding agents into real teammates. Assign tasks, track progress, compound skills.",
  icons: {
    icon: [{ url: "/favicon.svg", type: "image/svg+xml" }],
    shortcut: ["/favicon.svg"],
    // iOS never reads the manifest's icons for the home screen; it needs its
    // own opaque, full-bleed square and rounds the corners itself.
    apple: [{ url: "/icons/apple-touch-icon.png", sizes: "180x180" }],
  },
  // Home-screen behaviour: launch without browser chrome, and label the icon
  // "Multica" rather than the long SEO <title>. `capable` renders the
  // standardised `mobile-web-app-capable` tag — Next 16 no longer emits the
  // deprecated apple-prefixed spelling, so iOS standalone rides on the
  // manifest's `display` instead (honoured since iOS 16.4).
  appleWebApp: {
    capable: true,
    title: "Multica",
    // `default` keeps the web view below the status bar, which iOS paints in
    // the theme colour with legible text. `black-translucent` would put the
    // page under it too, but always draws the status text white, unreadable
    // on the light themes.
    statusBarStyle: "default",
  },
  openGraph: {
    type: "website",
    siteName: "Multica",
    locale: "en_US",
  },
  twitter: {
    card: "summary_large_image",
    site: "@multica_hq",
    creator: "@multica_hq",
  },
  alternates: {
    canonical: "/",
  },
  robots: {
    index: true,
    follow: true,
  },
};

export default async function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  const locale = await getRequestLocale();
  const apiBaseUrl = resolveBrowserApiBaseUrl(process.env);
  const wsUrl = resolveBrowserWsUrl(process.env);
  // Opt-in Google Analytics: omitted entirely unless GOOGLE_ANALYTICS_ID is set.
  const googleAnalyticsId = resolveGoogleAnalyticsId(process.env);
  // Dev toolbar: a status bar pinned to the bottom of the window. Opt-in per
  // developer with VITE_DEV_TOOLBAR=1, the same convention and var prefix as
  // react-grab below, so one variable enables it on web and desktop. Without
  // an explicit API URL the browser goes through the dev proxy, so the
  // toolbar reports the proxy's upstream.
  const devToolbarApiUrl =
    process.env.NODE_ENV === "development" && process.env.VITE_DEV_TOOLBAR
      ? apiBaseUrl || resolveDevRemoteApiUrl(process.env)
      : undefined;

  return (
    <html
      lang={HTML_LANG[locale]}
      suppressHydrationWarning
      className="antialiased font-sans h-full"
    >
      <body className="h-full overflow-hidden">
        {/*
          react-grab: dev-only element inspector. Hold ⌘C (Mac) / Ctrl+C and click
          any element to copy its source path + line + component stack for pasting
          to an AI. Opt-in per developer: only loads when VITE_REACT_GRAB is set in
          a local, gitignored apps/web/.env.local — it never activates for anyone
          else. Both guards are read server-side, so the <Script> is omitted from
          the HTML entirely unless you opted in. The VITE_ prefix is shared with the
          desktop renderer (apps/desktop/src/renderer/src/main.tsx), where Vite only
          exposes VITE_-prefixed vars to client code, so one var name covers both
          apps. See https://www.react-grab.com/
        */}
        {process.env.NODE_ENV === "development" && process.env.VITE_REACT_GRAB && (
          <Script
            src="//unpkg.com/react-grab/dist/index.global.js"
            crossOrigin="anonymous"
            strategy="beforeInteractive"
          />
        )}
        <ThemeProvider>
          <WebProviders
            locale={locale}
            apiBaseUrl={apiBaseUrl}
            wsUrl={wsUrl}
            devToolbarApiUrl={devToolbarApiUrl}
          >
            {children}
          </WebProviders>
          <Toaster />
        </ThemeProvider>
        {googleAnalyticsId && <GoogleAnalytics measurementId={googleAnalyticsId} />}
      </body>
    </html>
  );
}
