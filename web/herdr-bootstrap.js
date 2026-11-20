const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.1-512-d22020af24cfd22a/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
