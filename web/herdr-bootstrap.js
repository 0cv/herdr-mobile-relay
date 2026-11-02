const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.24.0-485-5b76ce14c26ec65b/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
