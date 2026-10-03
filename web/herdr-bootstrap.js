const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.4-406-3b0b7f7171537713/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
