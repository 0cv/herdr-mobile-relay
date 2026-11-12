const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-494-235a635f16e3e489/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
