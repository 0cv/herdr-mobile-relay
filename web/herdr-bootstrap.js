const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.3-387-aaf0023a53a0b87b/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
