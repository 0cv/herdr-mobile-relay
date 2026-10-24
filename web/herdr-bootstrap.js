const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.0-460-855d152fd69b9b8a/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
