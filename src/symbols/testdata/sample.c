#define LIMIT 10
#define SQUARE(x) ((x) * (x))

struct point {
    int x;
};

typedef struct point point_t;

enum color { RED };

static int add(int a, int b) {
    return a + b;
}

char *name(void) {
    return "x";
}
