import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;

/** Shared Java/Go contract. Does not validate token identity or expiration. */
public final class TokenDigest {
    public static String digest(String authorization) {
        if (authorization == null) throw new IllegalArgumentException("empty token");
        String token = authorization.startsWith("Bearer ")
                ? authorization.substring(7) : authorization;
        if (token.isEmpty()) throw new IllegalArgumentException("empty token");
        try {
            byte[] hash = MessageDigest.getInstance("SHA-256")
                    .digest(token.getBytes(StandardCharsets.UTF_8));
            StringBuilder result = new StringBuilder(64);
            for (byte b : hash) {
                result.append(Character.forDigit((b >>> 4) & 15, 16));
                result.append(Character.forDigit(b & 15, 16));
            }
            return result.toString();
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException(e);
        }
    }

    public static void main(String[] args) {
        String expected = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad";
        if (!digest("abc").equals(expected) || !digest("Bearer abc").equals(expected)
                || digest("ABC").equals(expected) || digest("abc\n").equals(expected)) {
            throw new AssertionError("Java/Go digest contract mismatch");
        }
        try { digest("Bearer "); throw new AssertionError("empty accepted"); }
        catch (IllegalArgumentException expectedException) { }
        System.out.println("Token digest contract OK");
    }
}
