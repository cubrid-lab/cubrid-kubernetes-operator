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

import java.io.BufferedWriter;
import java.io.IOException;
import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.InvalidPathException;
import java.nio.file.Path;
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
 * capabilities         print application capabilities without opening a database
 * </pre>
 *
 * <p>Settings come from the environment: JDBC_URL (required), DB_USER (default dba), DB_PASSWORD,
 * CLIENT_ID (default c1), OPS (default 20), START_SEQ (default 1), INTERVAL_MS (default 0),
 * ROLLBACK_EVERY (default 5: every fifth operation is rolled back; 0 for none), READ_EVERY (default
 * 0 for none: with N, every Nth operation reads back the row of the client's last acknowledged
 * insert instead of writing; it takes precedence over ROLLBACK_EVERY), ENDPOINT (default rw, copied
 * into the history). APP_RECONNECT_POLICY=stop-after-error requires run and an explicit writable
 * APP_CONNECTION_DIAGNOSTICS path. It stops application opens after a JDBC error; driver-internal
 * reconnects and retries remain outside this policy. A diagnostic path also works with default
 * reconnect behavior, without changing the history channel.
 */
public final class Workload {
  /** The note of a row that is written and then rolled back. */
  static final String ROLLED_BACK_NOTE = "must-not-exist";

  /** Starts the error of a read that found its row missing or different (test/workload). */
  static final String READ_MISMATCH = "read mismatch:";

  private static final PrintStream OUT = new PrintStream(System.out, true, StandardCharsets.UTF_8);

  private final String url = requireEnv("JDBC_URL");
  private final String user = env("DB_USER", "dba");
  private final String password = env("DB_PASSWORD", "");
  private final String client = env("CLIENT_ID", "c1");
  private final String endpoint = env("ENDPOINT", "rw");
  private Connection connection;
  private final boolean stopAfterError;
  private final BufferedWriter diagnostics;
  private int connectionAttempts;
  private String stage = "driver_open";
  private boolean policyStopped;
  private boolean diagnosticFailure;

  private Workload(boolean stopAfterError) throws IOException {
    this.stopAfterError = stopAfterError;
    String path = env("APP_CONNECTION_DIAGNOSTICS", "");
    diagnostics =
        path.isEmpty() ? null : Files.newBufferedWriter(Path.of(path), StandardCharsets.UTF_8);
  }

  private static final class DiagnosticFailure extends RuntimeException {
    private static final long serialVersionUID = 1L;
  }

  /** The last acknowledged insert of this client: its operation ID, sequence, amount and note. */
  private String lastOpId;

  private int lastSeq;
  private int lastAmount;
  private String lastNote;

  public static void main(String[] args) throws Exception {
    if (args.length == 0) {
      usage();
    }
    if (args[0].equals("capabilities")) {
      OUT.println(
          "{\"schemaVersion\":1,\"capabilities\":[\"application-reconnect-stop-after-error-v1\","
              + "\"application-connection-diagnostics-v1\"],\"physicalSessionContinuity\":\"unverified\","
              + "\"driverInternalRetryControl\":false}");
      return;
    }
    String policy = env("APP_RECONNECT_POLICY", "reconnect");
    boolean stop = policy.equals("stop-after-error");
    if ((!stop && !policy.equals("reconnect"))
        || (stop && (!args[0].equals("run") || env("APP_CONNECTION_DIAGNOSTICS", "").isEmpty()))) {
      System.err.println("Invalid application reconnect policy or required diagnostics configuration");
      System.exit(2);
    }
    Workload w;
    try {
      w = new Workload(stop);
    } catch (IOException | InvalidPathException e) {
      System.err.println("Could not open application connection diagnostics");
      System.exit(1);
      return;
    }
    try {
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
    } catch (DiagnosticFailure e) {
      w.diagnosticFailure = true;
    } finally {
      // Existing commands without diagnostics retain their original connection lifetime.
      if (w.diagnostics != null) {
        w.discard();
        try {
          w.diagnostics.close();
        } catch (IOException e) {
          w.diagnosticFailure = true;
        }
      }
    }
    if (w.diagnosticFailure) {
      System.err.println("Application connection diagnostics are incomplete");
      System.exit(1);
    }
    if (w.policyStopped) {
      System.exit(3);
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
    int readEvery = Integer.parseInt(env("READ_EVERY", "0"));
    for (int seq = start; seq < start + ops; seq++) {
      boolean rollback = rollbackEvery > 0 && seq % rollbackEvery == 0;
      if (readEvery > 0 && seq % readEvery == 0 && lastOpId != null) {
        read(seq);
      } else {
        operation(seq, rollback);
      }
      if (policyStopped || diagnosticFailure) {
        return;
      }
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
      stage = "statement";
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
        stage = "rollback";
        c.rollback();
        event(seq, op, opId, "acknowledged", 0, null, null);
        return;
      }
    } catch (SQLException e) {
      sqlFailure(e);
      event(seq, op, opId, "failed", 0, null, describe(e));
      return;
    }

    // Once the commit is sent, an error leaves the outcome unknown: the server may have
    // committed before the answer was lost.
    try {
      stage = "commit";
      connection.commit();
      event(seq, op, opId, "acknowledged", 0, null, null);
      lastOpId = opId;
      lastSeq = seq;
      lastAmount = amount;
      lastNote = note;
    } catch (SQLException e) {
      sqlFailure(e);
      event(seq, op, opId, "unknown", 0, null, describe(e));
    }
  }

  /**
   * Reads back the row of the last acknowledged insert and compares it with what was sent. It is
   * acknowledged only when the row is there with those values. A row that is missing or differs
   * fails with an error that starts with READ_MISMATCH; a SQL error fails with the error.
   */
  private void read(int seq) {
    String op = "read";
    String opId = String.format("%s-%06d", client, seq);
    String target = lastOpId;
    event(seq, op, opId, "attempted", lastAmount, lastNote, null, target);
    String mismatch;
    try {
      Connection c = connect();
      stage = "read";
      try (PreparedStatement s =
          c.prepareStatement("SELECT client_id, seq, amount, note FROM ledger WHERE op_id = ?")) {
        s.setString(1, target);
        try (ResultSet r = s.executeQuery()) {
          if (!r.next()) {
            mismatch = target + " not found";
          } else {
            String found =
                r.getString(1) + "/" + r.getInt(2) + "/" + r.getInt(3) + "/" + r.getString(4);
            String sent = client + "/" + lastSeq + "/" + lastAmount + "/" + lastNote;
            mismatch = found.equals(sent) ? null : target + " has " + found + ", sent " + sent;
          }
        }
      }
      try {
        stage = "commit";
        c.commit();
      } catch (SQLException e) {
        // What the SELECT found stays the evidence, also when ending the read fails.
        sqlFailure(e);
        String error = mismatch != null ? READ_MISMATCH + " " + mismatch : describe(e);
        event(seq, op, opId, "failed", 0, null, error);
        return;
      }
    } catch (SQLException e) {
      sqlFailure(e);
      event(seq, op, opId, "failed", 0, null, describe(e));
      return;
    }
    if (mismatch != null) {
      event(seq, op, opId, "failed", 0, null, READ_MISMATCH + " " + mismatch);
      return;
    }
    event(seq, op, opId, "acknowledged", 0, null, null);
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

  /** Returns the Java Connection; driver-internal reconnects and retries are not controlled. */
  private Connection connect() throws SQLException {
    if (connection == null) {
      if (diagnosticFailure || policyStopped) {
        throw new DiagnosticFailure();
      }
      stage = "driver_open";
      connectionAttempts++;
      diagnostic("application_connection_attempted", null);
      if (diagnosticFailure) {
        throw new DiagnosticFailure();
      }
      connection = DriverManager.getConnection(url, user, password);
      diagnostic("application_connection_returned", null);
      if (diagnosticFailure) {
        throw new DiagnosticFailure();
      }
      stage = "configure";
      connection.setAutoCommit(false);
    }
    return connection;
  }

  private void sqlFailure(SQLException error) {
    diagnostic("application_connection_failed", error);
    discard();
    if (stopAfterError) {
      policyStopped = true;
      diagnostic("application_reconnect_policy_stopped", null);
    }
  }

  private void discard() {
    if (connection != null) {
      diagnostic("application_connection_close_attempted", null);
      try {
        connection.close();
        diagnostic("application_connection_closed", null);
      } catch (SQLException error) {
        // A best-effort close must not replace the operation's failed or unknown outcome.
        diagnostic("application_connection_close_failed", error);
      }
      connection = null;
    }
  }

  private void diagnostic(String event, SQLException error) {
    if (diagnostics == null || diagnosticFailure) {
      return;
    }
    StringBuilder line = new StringBuilder(240);
    line.append("{\"schemaVersion\":1,\"t\":").append(quote(Instant.now().toString()));
    line.append(",\"client\":").append(quote(client));
    line.append(",\"applicationConnectionAttempt\":").append(connectionAttempts);
    line.append(",\"event\":").append(quote(event));
    line.append(",\"stage\":").append(quote(stage));
    if (error != null) {
      line.append(",\"sqlState\":").append(quote(safeSQLState(error)));
      line.append(",\"vendorCode\":").append(error.getErrorCode());
    }
    try {
      diagnostics.write(line.append('}').toString());
      diagnostics.newLine();
      diagnostics.flush();
    } catch (IOException e) {
      diagnosticFailure = true;
    }
  }

  private static String safeSQLState(SQLException error) {
    String state = error.getSQLState();
    return state != null && state.matches("[A-Z0-9]{5}") ? state : "unknown";
  }

  private void event(
      int seq, String op, String opId, String event, int amount, String note, String error) {
    event(seq, op, opId, event, amount, note, error, null);
  }

  private void event(
      int seq,
      String op,
      String opId,
      String event,
      int amount,
      String note,
      String error,
      String target) {
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
    if (target != null) {
      line.append(",\"target\":").append(quote(target));
    }
    if (error != null) {
      line.append(",\"error\":").append(quote(error));
    }
    OUT.println(line.append('}'));
  }

  private String describe(SQLException e) {
    if (stopAfterError) {
      return "SQLState " + safeSQLState(e) + ", code " + e.getErrorCode();
    }
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
