const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.10-410-2b3d95930bb85b5f/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
