// The Linux headless WPE compositor crashes on library-all (SIGSEGV with
// recursive native frames). Use the GTK backend on an isolated Xvfb display,
// with the same WebKit revision, fixtures, assertions and locales. Chromium
// keeps its normal launch options. Preload only in the server GUI checks.
const { webkit } = require("playwright");
if (process.platform !== "linux" || !process.env.DISPLAY) {
  throw new Error("Server GUI checks require Linux and an isolated Xvfb display");
}
const launch = webkit.launch.bind(webkit);
webkit.launch = (options = {}) => launch({
  ...options,
  headless: false,
  env: options.env ?? { ...process.env, LANG: "en_US.UTF-8", LC_ALL: "en_US.UTF-8" },
});
