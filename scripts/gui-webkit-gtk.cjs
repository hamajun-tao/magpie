// The headless WPE compositor crashes natively on library-all. Keep the
// same WebKit revision on GTK, with a private display for every browser:
// parallel windows on one X server compete for native pointer focus.
const { spawn } = require("node:child_process");
const { webkit } = require("playwright");
if (process.platform !== "linux") throw new Error("Server GTK checks require Linux");
const displays = new Set();
process.on("exit", () => { for (const child of displays) child.kill(); });
async function display() {
  const child = spawn("/usr/bin/Xvfb", ["-displayfd", "3", "-screen", "0", "3840x2160x24", "-nolisten", "tcp"], {
    stdio: ["ignore", "ignore", "ignore", "pipe"],
  });
  displays.add(child);
  try {
    const number = await new Promise((resolve, reject) => {
      let value = "";
      const timer = setTimeout(() => reject(new Error("Xvfb startup timed out")), 10000);
      const finish = (error, result) => { clearTimeout(timer); error ? reject(error) : resolve(result); };
      child.once("error", error => finish(error));
      child.once("exit", code => finish(new Error(`Xvfb exited before launch: ${code}`)));
      child.stdio[3].on("data", chunk => {
        value += chunk;
        if (value.includes("\n")) {
          const number = value.trim();
          if (!/^\d+$/.test(number)) finish(new Error("Xvfb returned an invalid display"));
          else finish(null, number);
        }
      });
    });
    return { name: `:${number}`, close: () => { displays.delete(child); child.kill(); } };
  } catch (error) {
    displays.delete(child); child.kill(); throw error;
  }
}
const launch = webkit.launch.bind(webkit);
webkit.launch = async (options = {}) => {
  const screen = await display();
  try {
    const browser = await launch({
      ...options, headless: false,
      env: { ...(options.env ?? { ...process.env, LANG: "en_US.UTF-8", LC_ALL: "en_US.UTF-8" }), DISPLAY: screen.name },
    });
    browser.once("disconnected", screen.close);
    return browser;
  } catch (error) { screen.close(); throw error; }
};
