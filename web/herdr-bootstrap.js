const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.4-402-c7ca5a55cfc6726b/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
