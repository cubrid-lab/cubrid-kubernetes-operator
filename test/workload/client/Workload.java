/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Statement;
import java.time.Instant;

/**
 * The SQL workload client of docs/testing/scenario-contract.md, on the official CUBRID JDBC driver.
 *
 * <p>It writes one JSON line per event to standard output: the history of the run, or with
 * {@code dump} the rows of the ledger table. Diagnostics go to standard error.
 *
 * <pre>
 * init                 create the tables when they do not exist
 * run                  run OPS operations as client CLIENT_ID and print the history
 * dump                 print every ledger row
 * marker NAME VALUE    write one row of the marker table
 * </pre>
 *
 * <p>Settings come from the environment: JDBC_URL (required), DB_USER (default dba), DB_PASSWORD,
 * CLIENT_ID (default c1), OPS (default 20), START_SEQ (default 1), INTERVAL_MS (default 0),
 * ROLLBACK_EVERY (default 5: every fifth operation is rolled back; 0 for none), ENDPOINT (default
 * rw, copied into the history).
 */
public final class Workload {
  /** The note of a row that is written and then rolled back. */
  static final String ROLLED_BACK_NOTE = "must-not-exist";

  private static final PrintStream OUT = new PrintStream(System.out, true, StandardCharsets.UTF_8);

  private final String url = requireEnv("JDBC_URL");
  private final String user = env("DB_USER", "dba");
  private final String password = env("DB_PASSWORD", "");
  private final String client = env("CLIENT_ID", "c1");
  private final String endpoint = env("ENDPOINT", "rw");
  private Connection connection;

  public static void main(String[] args) throws Exception {
    if (args.length == 0) {
      usage();
    }
    Workload w = new Workload();
    switch (args[0]) {
      case "init" -> w.init();
      case "run" -> w.run();
      case "dump" -> w.dump();
      case "marker" -> {
        if (args.length != 3) {
          usage();
        }
        w.marker(args[1], args[2]);
      }
      default -> usage();
    }
  }

  private static void usage() {
    System.err.println("usage: Workload init | run | dump | marker NAME VALUE");
    System.exit(2);
  }

  /** Creates the tables of the contract. Both have a primary key, as replication needs. */
  private void init() throws SQLException {
    Connection c = connect();
    if (!tableExists(c, "ledger")) {
      try (Statement s = c.createStatement()) {
        s.executeUpdate(
            "CREATE TABLE ledger (op_id VARCHAR(40) PRIMARY KEY, client_id VARCHAR(16) NOT NULL,"
                + " seq INT NOT NULL, amount INT NOT NULL, note VARCHAR(64))");
        s.executeUpdate("CREATE UNIQUE INDEX ledger_client_seq ON ledger (client_id, seq)");
      }
    }
    if (!tableExists(c, "marker")) {
      try (Statement s = c.createStatement()) {
        s.executeUpdate(
            "CREATE TABLE marker (name VARCHAR(32) PRIMARY KEY, [value] VARCHAR(64) NOT NULL)");
      }
    }
    c.commit();
    System.err.println("tables are present");
  }

  private static boolean tableExists(Connection c, String name) throws SQLException {
    try (PreparedStatement s = c.prepareStatement("SELECT 1 FROM db_class WHERE class_name = ?")) {
      s.setString(1, name);
      try (ResultSet r = s.executeQuery()) {
        return r.next();
      }
    }
  }

  /**
   * Runs the numbered operations. Every operation is one transaction and leaves an "attempted"
   * line before anything is sent and exactly one line with its outcome afterwards.
   */
  private void run() throws InterruptedException {
    int ops = Integer.parseInt(env("OPS", "20"));
    int start = Integer.parseInt(env("START_SEQ", "1"));
    long interval = Long.parseLong(env("INTERVAL_MS", "0"));
    int rollbackEvery = Integer.parseInt(env("ROLLBACK_EVERY", "5"));
    for (int seq = start; seq < start + ops; seq++) {
      boolean rollback = rollbackEvery > 0 && seq % rollbackEvery == 0;
      operation(seq, rollback);
      if (interval > 0) {
        Thread.sleep(interval);
      }
    }
  }

  private void operation(int seq, boolean rollback) {
    String op = rollback ? "rollback" : "insert";
    String opId = String.format("%s-%06d", client, seq);
    int amount = seq * 37 % 1000 + 1;
    String note = rollback ? ROLLED_BACK_NOTE : client + "-" + seq;
    event(seq, op, opId, "attempted", amount, note, null);

    // Until the commit is sent, a failure means that nothing was committed.
    try {
      Connection c = connect();
      try (PreparedStatement s =
          c.prepareStatement(
              "INSERT INTO ledger (op_id, client_id, seq, amount, note) VALUES (?, ?, ?, ?, ?)")) {
        s.setString(1, opId);
        s.setString(2, client);
        s.setInt(3, seq);
        s.setInt(4, amount);
        s.setString(5, note);
        s.executeUpdate();
      }
      if (rollback) {
        c.rollback();
        event(seq, op, opId, "acknowledged", 0, null, null);
        return;
      }
    } catch (SQLException e) {
      discard();
      event(seq, op, opId, "failed", 0, null, describe(e));
      return;
    }

    // Once the commit is sent, an error leaves the outcome unknown: the server may have
    // committed before the answer was lost.
    try {
      connection.commit();
      event(seq, op, opId, "acknowledged", 0, null, null);
    } catch (SQLException e) {
      discard();
      event(seq, op, opId, "unknown", 0, null, describe(e));
    }
  }

  private void dump() throws SQLException {
    Connection c = connect();
    try (Statement s = c.createStatement();
        ResultSet r =
            s.executeQuery(
                "SELECT op_id, client_id, seq, amount, note FROM ledger ORDER BY client_id, seq, op_id")) {
      while (r.next()) {
        OUT.println(
            "{\"opId\":"
                + quote(r.getString(1))
                + ",\"client\":"
                + quote(r.getString(2))
                + ",\"seq\":"
                + r.getInt(3)
                + ",\"amount\":"
                + r.getInt(4)
                + ",\"note\":"
                + quote(r.getString(5))
                + "}");
      }
    }
    c.commit();
  }

  private void marker(String name, String value) throws SQLException {
    Connection c = connect();
    try (PreparedStatement s =
        c.prepareStatement("INSERT INTO marker (name, [value]) VALUES (?, ?)")) {
      s.setString(1, name);
      s.setString(2, value);
      s.executeUpdate();
    }
    c.commit();
    System.err.println("marker " + name + " written");
  }

  /** Returns the open connection, or opens one. A connection that failed is not reused. */
  private Connection connect() throws SQLException {
    if (connection == null) {
      Connection c = DriverManager.getConnection(url, user, password);
      c.setAutoCommit(false);
      connection = c;
    }
    return connection;
  }

  private void discard() {
    if (connection != null) {
      try {
        connection.close();
      } catch (SQLException ignored) {
        // The connection is being thrown away because it failed.
      }
      connection = null;
    }
  }

  private void event(
      int seq, String op, String opId, String event, int amount, String note, String error) {
    StringBuilder line = new StringBuilder(160);
    line.append("{\"t\":").append(quote(Instant.now().toString()));
    line.append(",\"client\":").append(quote(client));
    line.append(",\"seq\":").append(seq);
    line.append(",\"op\":").append(quote(op));
    line.append(",\"opId\":").append(quote(opId));
    line.append(",\"endpoint\":").append(quote(endpoint));
    line.append(",\"event\":").append(quote(event));
    if (note != null) {
      line.append(",\"amount\":").append(amount);
      line.append(",\"note\":").append(quote(note));
    }
    if (error != null) {
      line.append(",\"error\":").append(quote(error));
    }
    OUT.println(line.append('}'));
  }

  private static String describe(SQLException e) {
    return "code " + e.getErrorCode() + ": " + e.getMessage();
  }

  /** Returns s as a JSON string. */
  static String quote(String s) {
    if (s == null) {
      return "null";
    }
    StringBuilder b = new StringBuilder(s.length() + 2).append('"');
    for (int i = 0; i < s.length(); i++) {
      char ch = s.charAt(i);
      switch (ch) {
        case '"' -> b.append("\\\"");
        case '\\' -> b.append("\\\\");
        case '\n' -> b.append("\\n");
        case '\r' -> b.append("\\r");
        case '\t' -> b.append("\\t");
        default -> {
          if (ch < 0x20) {
            b.append(String.format("\\u%04x", (int) ch));
          } else {
            b.append(ch);
          }
        }
      }
    }
    return b.append('"').toString();
  }

  private static String env(String name, String fallback) {
    String v = System.getenv(name);
    return v == null || v.isEmpty() ? fallback : v;
  }

  private static String requireEnv(String name) {
    String v = System.getenv(name);
    if (v == null || v.isEmpty()) {
      System.err.println(name + " is required");
      System.exit(2);
    }
    return v;
  }
}
