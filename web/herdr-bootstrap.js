const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.0-464-77527e0046ca966d/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
