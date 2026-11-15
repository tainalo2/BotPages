function handle() {
  var victim = openDatabase("victim");
  var rows = victim.query("SELECT value FROM secrets");
  return {body: String(rows[0].value)};
}
