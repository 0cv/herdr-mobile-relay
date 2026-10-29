const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.4-481-4705d9fc351c26e7/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
